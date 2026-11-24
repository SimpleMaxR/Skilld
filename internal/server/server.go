package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"time"

	"skilld/internal/asyncwrite"
	"skilld/internal/db"
	"skilld/internal/hotstate"
	"skilld/internal/skill"
	"skilld/internal/store"
)

const (
	Version           = "0.2.0"
	OnlineThresholdMs = 65_000
	maxBodyBytes      = 16 * 1024 * 1024
)

// Options wires together all core subsystems of the Skilld server.
type Options struct {
	SplitDB     *db.SplitDB
	Store       *store.Store
	Engine      *hotstate.Engine
	Writer      *asyncwrite.GroupCommitWriter
	GitHub      *skill.GitHubClient
	WebAssets   fs.FS
	GitHubToken string
}

// Server exposes a zero-middleware net/http handler backed by hotstate, SplitDB, and GroupCommitWriter.
type Server struct {
	db        *db.SplitDB
	store     *store.Store
	engine    *hotstate.Engine
	writer    *asyncwrite.GroupCommitWriter
	github    *skill.GitHubClient
	webAssets fs.FS
	mux       *http.ServeMux
}

type ctxKey string

const reservationCtxKey ctxKey = "reservation"

// New constructs the HTTP server and registers all static and API routes.
func New(opts Options) *Server {
	gh := opts.GitHub
	if gh == nil {
		gh = skill.NewGitHubClient(opts.GitHubToken)
	}
	s := &Server{
		db:        opts.SplitDB,
		store:     opts.Store,
		engine:    opts.Engine,
		writer:    opts.Writer,
		github:    gh,
		webAssets: opts.WebAssets,
		mux:       http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

func (s *Server) registerRoutes() {
	// Overview & Auth
	s.mux.HandleFunc("GET /api/auth/verify", s.handleAuthVerify)
	s.mux.HandleFunc("GET /api/overview", s.handleOverview)

	// Vault Skills
	s.mux.HandleFunc("GET /api/skills", s.handleListSkills)
	s.mux.HandleFunc("POST /api/skills", s.handleCreateSkill)
	s.mux.HandleFunc("POST /api/skills/import-github", s.handleImportGitHub)
	s.mux.HandleFunc("POST /api/skills/check-all-upstream", s.handleCheckAllUpstream)
	s.mux.HandleFunc("GET /api/skills/{id}", s.handleGetSkill)
	s.mux.HandleFunc("PUT /api/skills/{id}", s.handleUpdateSkill)
	s.mux.HandleFunc("DELETE /api/skills/{id}", s.handleDeleteSkill)
	s.mux.HandleFunc("POST /api/skills/{id}/versions", s.handleAddSkillVersion)
	s.mux.HandleFunc("POST /api/skills/{id}/check-upstream", s.handleCheckSingleUpstream)
	s.mux.HandleFunc("POST /api/skills/{id}/upgrade-upstream", s.handleUpgradeUpstream)
	s.mux.HandleFunc("GET /api/versions/{id}", s.handleGetVersion)

	// Devices & Daemon Protocol
	s.mux.HandleFunc("GET /api/devices", s.handleListDevices)
	s.mux.HandleFunc("DELETE /api/devices/{id}", s.handleDeleteDevice)
	s.mux.HandleFunc("POST /api/devices/report", s.handleDeviceReport)
	s.mux.HandleFunc("POST /api/devices/{id}/actions/claim", s.handleClaimActions)
	s.mux.HandleFunc("POST /api/actions/{id}/complete", s.handleCompleteAction)

	// Installations & Diff
	s.mux.HandleFunc("GET /api/installations/diff", s.handleInstallationDiff)
	s.mux.HandleFunc("POST /api/installations/deploy", s.handleDeployInstallations)
	s.mux.HandleFunc("POST /api/installations/update", s.handleUpdateInstallation)
	s.mux.HandleFunc("POST /api/installations/reset", s.handleResetInstallation)
	s.mux.HandleFunc("POST /api/installations/promote", s.handlePromoteInstallation)
	s.mux.HandleFunc("POST /api/installations/adopt", s.handleAdoptInstallation)
	s.mux.HandleFunc("POST /api/installations/ignore", s.handleIgnoreInstallation)
	s.mux.HandleFunc("POST /api/installations/remove", s.handleRemoveInstallation)

	// Actions
	s.mux.HandleFunc("GET /api/actions", s.handleListActions)
	s.mux.HandleFunc("POST /api/actions/{id}/cancel", s.handleCancelAction)

	// High-Concurrency Engine, Quotas & Async Telemetry
	s.mux.HandleFunc("GET /api/system/runtime", s.handleSystemRuntime)
	s.mux.HandleFunc("GET /api/system/config", s.handleGetSystemConfig)
	s.mux.HandleFunc("PUT /api/system/config", s.handlePutSystemConfig)
	s.mux.HandleFunc("GET /api/accounts", s.handleListAccounts)
	s.mux.HandleFunc("POST /api/accounts", s.handleCreateAccount)
	s.mux.HandleFunc("POST /api/accounts/{id}/topup", s.handleTopUpAccount)
	s.mux.HandleFunc("DELETE /api/accounts/{id}", s.handleDeleteAccount)
	s.mux.HandleFunc("GET /api/audit-logs", s.handleAuditLogs)
}

// Handler returns the root http.Handler with built-in CORS, hot-path admission, and async logging.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setCORSHeaders(w.Header())
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		now := time.Now()
		reqIP := extractClientIP(r)

		// Unauthenticated endpoints: /api/health and embedded web assets.
		if !strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api/health" {
			if !s.engine.AllowUnauthIP(reqIP, now) {
				writeError(w, http.StatusTooManyRequests, "RATE_LIMITED", "Too many requests from client IP")
				return
			}
			if r.URL.Path == "/api/health" {
				if r.Method != http.MethodGet && r.Method != http.MethodHead {
					writeError(w, http.StatusNotFound, "NOT_FOUND", "API route not found")
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{
					"ok":                true,
					"runtime":           "go-sqlite",
					"version":           Version,
					"time":              skill.ISONow(),
					"walAutocheckpoint": db.WalAutocheckpoint,
					"queueCapacity":     asyncwrite.QueueCapacity,
				})
				return
			}
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				if s.serveEmbeddedAsset(w, r) {
					return
				}
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			if r.Method != http.MethodHead {
				_, _ = w.Write([]byte("Not Found"))
			}
			return
		}

		// Protected /api/* hot path: classify route, authenticate, rate-limit, and pre-deduct quota in memory.
		_, pattern := s.mux.Handler(r)
		routeKey := pattern
		if routeKey == "" {
			routeKey = r.Method + " " + r.URL.Path
		}

		cfg := s.engine.GetConfig()
		costMicros := classifyRouteCost(r.Method, r.URL.Path, cfg)
		token := extractBearerToken(r.Header.Get("Authorization"))

		res, admitErr := s.engine.AdmitRequest(token, routeKey, costMicros, now)
		reqID := skill.GenerateID("req")
		if admitErr != nil {
			s.writer.Enqueue(asyncwrite.WriteEvent{
				Kind:       asyncwrite.EventAccessLog,
				RequestID:  reqID,
				AccountID:  "unauthenticated",
				Method:     r.Method,
				Path:       r.URL.Path,
				Route:      routeKey,
				StatusCode: admitErr.Status,
				LatencyUs:  time.Since(now).Microseconds(),
				CostMicros: 0,
				ClientIP:   reqIP,
				CreatedAt:  skill.ISONow(),
			})
			writeError(w, admitErr.Status, admitErr.Code, admitErr.Message)
			return
		}

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		ctx := context.WithValue(r.Context(), reservationCtxKey, res)

		func() {
			defer func() {
				if recVal := recover(); recVal != nil {
					s.engine.RollbackReservation(res)
					rec.status = http.StatusInternalServerError
					writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", fmt.Sprintf("Internal error: %v", recVal))
				}
			}()
			if pattern == "" {
				writeError(rec, http.StatusNotFound, "NOT_FOUND", "API route not found")
				return
			}
			s.mux.ServeHTTP(rec, r.WithContext(ctx))
		}()

		latencyUs := time.Since(now).Microseconds()
		nowISO := skill.ISONow()

		var charged int64
		if rec.status >= 500 {
			s.engine.RollbackReservation(res)
		} else {
			var balAfter int64
			balAfter, charged = s.engine.CommitReservation(res, rec.status, costMicros)
			if charged > 0 {
				s.writer.Enqueue(asyncwrite.WriteEvent{
					Kind:         asyncwrite.EventQuotaDeduct,
					RequestID:    reqID,
					AccountID:    res.Account.ID,
					Unlimited:    res.Account.UnlimitedQuota,
					Route:        routeKey,
					CostMicros:   charged,
					BalanceAfter: balAfter,
					CreatedAt:    nowISO,
				})
			}
		}

		s.writer.Enqueue(asyncwrite.WriteEvent{
			Kind:       asyncwrite.EventAccessLog,
			RequestID:  reqID,
			AccountID:  res.Account.ID,
			Method:     r.Method,
			Path:       r.URL.Path,
			Route:      routeKey,
			StatusCode: rec.status,
			LatencyUs:  latencyUs,
			CostMicros: charged,
			ClientIP:   reqIP,
			CreatedAt:  nowISO,
		})
	})
}

func classifyRouteCost(method, path string, cfg hotstate.SystemDynamicConfig) int64 {
	if strings.Contains(path, "upstream") || path == "/api/skills/import-github" {
		if cfg.UpstreamCostMicros > 0 {
			return cfg.UpstreamCostMicros
		}
		return 20
	}
	if method == http.MethodGet || method == http.MethodHead {
		if cfg.ReadCostMicros > 0 {
			return cfg.ReadCostMicros
		}
		return 1
	}
	if cfg.WriteCostMicros > 0 {
		return cfg.WriteCostMicros
	}
	return 5
}

func (s *Server) serveEmbeddedAsset(w http.ResponseWriter, r *http.Request) bool {
	if s.webAssets == nil {
		return false
	}
	path := r.URL.Path
	if path == "" || path == "/" {
		path = "/index.html"
	}
	var fsPath, contentType string
	switch path {
	case "/index.html":
		fsPath = "packages/web/index.html"
		contentType = "text/html; charset=utf-8"
	case "/styles.css":
		fsPath = "packages/web/styles.css"
		contentType = "text/css; charset=utf-8"
	case "/app.mjs":
		fsPath = "packages/web/app.mjs"
		contentType = "application/javascript; charset=utf-8"
	default:
		return false
	}
	data, err := fs.ReadFile(s.webAssets, fsPath)
	if err != nil {
		return false
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
	return true
}

// --- Handlers ---

func (s *Server) handleAuthVerify(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type enrichedDevice struct {
	store.Device
	Online         bool `json:"online"`
	SkillCount     int  `json:"skillCount"`
	AttentionCount int  `json:"attentionCount"`
}

func (s *Server) enrichDevicesWithHotState(devices []store.Device, installations []store.Installation) []enrichedDevice {
	nowMs := time.Now().UnixMilli()
	out := make([]enrichedDevice, 0, len(devices))
	for _, d := range devices {
		if hotMs, ok := s.engine.GetDeviceLastSeenMs(d.ID); ok {
			hotISO := time.UnixMilli(hotMs).UTC().Format("2006-01-02T15:04:05.000Z")
			if hotISO > d.LastSeenAt {
				d.LastSeenAt = hotISO
			}
		}
		lastSeenMs := parseISOMillis(d.LastSeenAt)
		online := lastSeenMs > 0 && (nowMs-lastSeenMs) < OnlineThresholdMs

		skillCount := 0
		attentionCount := 0
		for _, inst := range installations {
			if inst.DeviceID != d.ID {
				continue
			}
			skillCount++
			if !inst.Ignored && inst.Status != skill.InstallStatusSynced {
				attentionCount++
			}
		}

		out = append(out, enrichedDevice{
			Device:         d,
			Online:         online,
			SkillCount:     skillCount,
			AttentionCount: attentionCount,
		})
	}
	return out
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	devices, err := s.store.ListDevices(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	skills, err := s.store.ListSkills(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	installations, err := s.store.ListInstallations(ctx, "", "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	actions, err := s.store.ListActions(ctx, "", "", 80)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}

	enrichedDevices := s.enrichDevicesWithHotState(devices, installations)

	onlineDevices := 0
	for _, d := range enrichedDevices {
		if d.Online {
			onlineDevices++
		}
	}

	upstreamUpdates := 0
	for i := range skills {
		if skills[i].HasUpstreamUpdate {
			upstreamUpdates++
		}
		for _, inst := range installations {
			if inst.VaultSkillID != nil && *inst.VaultSkillID == skills[i].ID {
				skills[i].InstalledCount++
				switch inst.Status {
				case skill.InstallStatusOutdated:
					skills[i].OutdatedCount++
				case skill.InstallStatusModified:
					skills[i].ModifiedCount++
				}
			}
		}
	}

	var synced, outdated, modified, unmanaged, missing int
	for _, inst := range installations {
		switch inst.Status {
		case skill.InstallStatusSynced:
			synced++
		case skill.InstallStatusOutdated:
			outdated++
		case skill.InstallStatusModified:
			modified++
		case skill.InstallStatusUnmanaged:
			unmanaged++
		case skill.InstallStatusMissing:
			missing++
		}
	}

	pendingActionCount := 0
	for _, a := range actions {
		if a.Status == skill.ActionStatusPending || a.Status == skill.ActionStatusClaimed {
			pendingActionCount++
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"devices":       enrichedDevices,
		"skills":        skills,
		"installations": installations,
		"actions":       actions,
		"summary": map[string]any{
			"totalDevices":       len(enrichedDevices),
			"onlineDevices":      onlineDevices,
			"totalSkills":        len(skills),
			"upstreamUpdates":    upstreamUpdates,
			"totalInstallations": len(installations),
			"syncedCount":        synced,
			"outdatedCount":      outdated,
			"modifiedCount":      modified,
			"unmanagedCount":     unmanaged,
			"missingCount":       missing,
			"pendingActionCount": pendingActionCount,
		},
	})
}

func (s *Server) handleListSkills(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	skills, err := s.store.ListSkills(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	installations, err := s.store.ListInstallations(ctx, "", "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	for i := range skills {
		for _, inst := range installations {
			if inst.VaultSkillID != nil && *inst.VaultSkillID == skills[i].ID {
				skills[i].InstalledCount++
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": skills})
}

func (s *Server) handleGetSkill(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sk, err := s.store.GetSkill(r.Context(), id, true, true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if sk == nil {
		writeError(w, http.StatusNotFound, "SKILL_NOT_FOUND", "Skill not found")
		return
	}
	insts, err := s.store.ListInstallations(r.Context(), "", sk.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skill": sk, "installations": insts})
}

func (s *Server) handleGetVersion(w http.ResponseWriter, r *http.Request) {
	ver, err := s.store.GetVersion(r.Context(), r.PathValue("id"), true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if ver == nil {
		writeError(w, http.StatusNotFound, "VERSION_NOT_FOUND", "Version not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": ver})
}

func (s *Server) handleCreateSkill(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID              string                     `json:"id"`
		Name            string                     `json:"name"`
		Description     *string                    `json:"description"`
		SourceType      string                     `json:"sourceType"`
		UpstreamURL     string                     `json:"upstreamUrl"`
		UpstreamRef     string                     `json:"upstreamRef"`
		UpstreamSubpath string                     `json:"upstreamSubpath"`
		Note            string                     `json:"note"`
		Files           map[string]skill.FileEntry `json:"files"`
		Markdown        *string                    `json:"markdown"`
	}
	if err := readJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}

	files, err := buildFilesInput(body.Files, body.Markdown)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	contentHash, err := skill.ComputeFilesHash(files)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}

	var meta skill.SkillMeta
	if md, ok := files["SKILL.md"]; ok && md.Encoding == "utf8" {
		meta = skill.ParseSkillFrontmatter(md.Content)
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = strings.TrimSpace(meta.Name)
	}
	if name == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "Skill name is required")
		return
	}

	desc := meta.Description
	if body.Description != nil {
		desc = *body.Description
	}
	desc = strings.TrimSpace(desc)

	var id string
	if strings.TrimSpace(body.ID) != "" {
		id = skill.SlugifySkillID(body.ID)
		existing, err := s.store.GetSkill(r.Context(), id, false, false)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
			return
		}
		if existing != nil {
			writeError(w, http.StatusConflict, "SKILL_EXISTS", fmt.Sprintf("Skill ID %q already exists", id))
			return
		}
	} else {
		id, err = s.allocateUniqueSkillID(r.Context(), name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
			return
		}
	}

	sourceType := body.SourceType
	if sourceType == "" {
		sourceType = "custom"
	}
	var upURL, upRef, upSub *string
	if strings.TrimSpace(body.UpstreamURL) != "" {
		parsed, err := skill.ParseGitHubURL(body.UpstreamURL, body.UpstreamRef, body.UpstreamSubpath)
		if err != nil {
			writeUpstreamOrBad(w, err)
			return
		}
		sourceType = "github"
		upURL = &parsed.CanonicalURL
		if parsed.Ref != "" {
			upRef = &parsed.Ref
		}
		if parsed.Subpath != "" {
			upSub = &parsed.Subpath
		}
	}

	created, err := s.store.CreateSkillWithVersion(r.Context(), store.CreateSkillInput{
		ID:              id,
		Name:            name,
		Description:     desc,
		SourceType:      sourceType,
		UpstreamURL:     upURL,
		UpstreamRef:     upRef,
		UpstreamSubpath: upSub,
		Note:            body.Note,
		Files:           files,
		ContentHash:     contentHash,
	})
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "SKILL_EXISTS", fmt.Sprintf("Skill ID %q already exists", id))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}

	s.engine.InvalidateAllDeviceDigests()
	writeJSON(w, http.StatusCreated, map[string]any{"skill": created})
}

func (s *Server) handleImportGitHub(w http.ResponseWriter, r *http.Request) {
	if s.engine.UpstreamCircuitOpen(time.Now()) {
		writeError(w, http.StatusServiceUnavailable, "UPSTREAM_CIRCUIT_OPEN", "GitHub upstream circuit breaker open")
		return
	}
	var body struct {
		URL         string  `json:"url"`
		Ref         string  `json:"ref"`
		Subpath     string  `json:"subpath"`
		Name        string  `json:"name"`
		Description *string `json:"description"`
		ID          string  `json:"id"`
		GitHubToken string  `json:"githubToken"`
	}
	if err := readJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}

	parsed, err := skill.ParseGitHubURL(body.URL, body.Ref, body.Subpath)
	if err != nil {
		writeUpstreamOrBad(w, err)
		return
	}

	fetched, err := s.github.FetchSkillFiles(r.Context(), parsed.Owner, parsed.Repo, parsed.Ref, parsed.Subpath, body.GitHubToken)
	if err != nil {
		s.engine.RecordUpstreamResult(false, time.Now())
		writeUpstreamOrBad(w, err)
		return
	}
	s.engine.RecordUpstreamResult(true, time.Now())

	fallbackName := parsed.Repo
	if parsed.Subpath != "" {
		segs := strings.Split(parsed.Subpath, "/")
		fallbackName = segs[len(segs)-1]
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = strings.TrimSpace(fetched.Meta.Name)
	}
	if name == "" {
		name = fallbackName
	}
	if name == "" {
		name = "github-skill"
	}

	desc := fetched.Meta.Description
	if body.Description != nil {
		desc = *body.Description
	}
	desc = strings.TrimSpace(desc)

	var id string
	if strings.TrimSpace(body.ID) != "" {
		id = skill.SlugifySkillID(body.ID)
		existing, err := s.store.GetSkill(r.Context(), id, false, false)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
			return
		}
		if existing != nil {
			writeError(w, http.StatusConflict, "SKILL_EXISTS", fmt.Sprintf("Skill ID %q already exists", id))
			return
		}
	} else {
		id, err = s.allocateUniqueSkillID(r.Context(), name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
			return
		}
	}

	shortRev := fetched.Rev
	if len(shortRev) > 7 {
		shortRev = shortRev[:7]
	}

	created, err := s.store.CreateSkillWithVersion(r.Context(), store.CreateSkillInput{
		ID:                    id,
		Name:                  name,
		Description:           desc,
		SourceType:            "github",
		UpstreamURL:           &parsed.CanonicalURL,
		UpstreamRef:           strPtrNonEmpty(parsed.Ref),
		UpstreamSubpath:       strPtrNonEmpty(parsed.Subpath),
		UpstreamLatestRev:     &fetched.Rev,
		UpstreamLatestMessage: &fetched.Message,
		UpstreamCheckedAt:     &fetched.CheckedAt,
		SourceRev:             &fetched.Rev,
		Note:                  fmt.Sprintf("Imported from GitHub (%s)", shortRev),
		Files:                 fetched.Files,
		ContentHash:           fetched.ContentHash,
	})
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "SKILL_EXISTS", fmt.Sprintf("Skill ID %q already exists", id))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}

	s.engine.InvalidateAllDeviceDigests()
	writeJSON(w, http.StatusCreated, map[string]any{"skill": created})
}

func (s *Server) handleUpdateSkill(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name            *string `json:"name"`
		Description     *string `json:"description"`
		UpstreamURL     *string `json:"upstreamUrl"`
		UpstreamRef     string  `json:"upstreamRef"`
		UpstreamSubpath string  `json:"upstreamSubpath"`
	}
	if err := readJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}

	patch := store.SkillMetaPatch{
		Name:        body.Name,
		Description: body.Description,
	}
	if body.UpstreamURL != nil {
		patch.UpstreamSet = true
		if strings.TrimSpace(*body.UpstreamURL) != "" {
			parsed, err := skill.ParseGitHubURL(*body.UpstreamURL, body.UpstreamRef, body.UpstreamSubpath)
			if err != nil {
				writeUpstreamOrBad(w, err)
				return
			}
			patch.UpstreamURL = &parsed.CanonicalURL
			patch.UpstreamRef = strPtrNonEmpty(parsed.Ref)
			patch.UpstreamSubpath = strPtrNonEmpty(parsed.Subpath)
		}
	}

	updated, err := s.store.UpdateSkillMeta(r.Context(), r.PathValue("id"), patch)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if updated == nil {
		writeError(w, http.StatusNotFound, "SKILL_NOT_FOUND", "Skill not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skill": updated})
}

func (s *Server) handleAddSkillVersion(w http.ResponseWriter, r *http.Request) {
	skillID := r.PathValue("id")
	existing, err := s.store.GetSkill(r.Context(), skillID, true, true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "SKILL_NOT_FOUND", "Skill not found")
		return
	}

	var body struct {
		Files          map[string]skill.FileEntry `json:"files"`
		Markdown       *string                    `json:"markdown"`
		Note           string                     `json:"note"`
		AllowDuplicate bool                       `json:"allowDuplicate"`
	}
	if err := readJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}

	files, err := buildFilesInput(body.Files, body.Markdown)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	contentHash, err := skill.ComputeFilesHash(files)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}

	if existing.CurrentContentHash != nil && *existing.CurrentContentHash == contentHash && !body.AllowDuplicate {
		writeJSON(w, http.StatusOK, map[string]any{
			"created": false,
			"version": existing.CurrentVersion,
			"skill":   existing,
		})
		return
	}

	note := strings.TrimSpace(body.Note)
	if note == "" {
		note = "Manual edit"
	}
	ver, err := s.store.AddSkillVersion(r.Context(), skillID, files, contentHash, nil, note)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	refreshed, err := s.store.GetSkill(r.Context(), skillID, true, true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}

	s.engine.InvalidateAllDeviceDigests()
	writeJSON(w, http.StatusCreated, map[string]any{
		"created": true,
		"version": ver,
		"skill":   refreshed,
	})
}

func (s *Server) handleCheckSingleUpstream(w http.ResponseWriter, r *http.Request) {
	sk, err := s.store.GetSkill(r.Context(), r.PathValue("id"), false, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if sk == nil {
		writeError(w, http.StatusNotFound, "SKILL_NOT_FOUND", "Skill not found")
		return
	}
	if sk.UpstreamURL == nil || *sk.UpstreamURL == "" {
		writeError(w, http.StatusBadRequest, "NO_UPSTREAM", "Skill does not have a GitHub upstream URL")
		return
	}
	updated, err := s.checkSingleSkillUpstream(r.Context(), sk)
	if err != nil {
		writeUpstreamOrBad(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skill": updated})
}

func (s *Server) handleCheckAllUpstream(w http.ResponseWriter, r *http.Request) {
	skills, err := s.store.ListSkills(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	results := make([]map[string]any, 0)
	for i := range skills {
		if skills[i].UpstreamURL == nil || *skills[i].UpstreamURL == "" {
			continue
		}
		updated, err := s.checkSingleSkillUpstream(r.Context(), &skills[i])
		if err != nil {
			results = append(results, map[string]any{
				"id":    skills[i].ID,
				"ok":    false,
				"error": err.Error(),
			})
		} else {
			results = append(results, map[string]any{
				"id":                skills[i].ID,
				"ok":                true,
				"hasUpstreamUpdate": updated.HasUpstreamUpdate,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (s *Server) handleUpgradeUpstream(w http.ResponseWriter, r *http.Request) {
	sk, err := s.store.GetSkill(r.Context(), r.PathValue("id"), true, true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if sk == nil {
		writeError(w, http.StatusNotFound, "SKILL_NOT_FOUND", "Skill not found")
		return
	}
	if sk.UpstreamURL == nil || *sk.UpstreamURL == "" {
		writeError(w, http.StatusBadRequest, "NO_UPSTREAM", "Skill does not have a GitHub upstream URL")
		return
	}

	parsed, err := skill.ParseGitHubURL(*sk.UpstreamURL, derefStr(sk.UpstreamRef), derefStr(sk.UpstreamSubpath))
	if err != nil {
		writeUpstreamOrBad(w, err)
		return
	}

	fetched, err := s.github.FetchSkillFiles(r.Context(), parsed.Owner, parsed.Repo, parsed.Ref, parsed.Subpath, "")
	if err != nil {
		s.engine.RecordUpstreamResult(false, time.Now())
		writeUpstreamOrBad(w, err)
		return
	}
	s.engine.RecordUpstreamResult(true, time.Now())

	if _, err := s.store.UpdateSkillUpstream(r.Context(), sk.ID, fetched.Rev, fetched.Message, fetched.CheckedAt); err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}

	if sk.CurrentContentHash != nil && *sk.CurrentContentHash == fetched.ContentHash {
		refreshed, err := s.store.GetSkill(r.Context(), sk.ID, true, true)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"upgraded": false, "skill": refreshed})
		return
	}

	shortRev := fetched.Rev
	if len(shortRev) > 7 {
		shortRev = shortRev[:7]
	}
	msg := fetched.Message
	if msg == "" {
		msg = "sync"
	}
	ver, err := s.store.AddSkillVersion(r.Context(), sk.ID, fetched.Files, fetched.ContentHash, &fetched.Rev, fmt.Sprintf("Upstream %s: %s", shortRev, msg))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	refreshed, err := s.store.GetSkill(r.Context(), sk.ID, true, true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	s.engine.InvalidateAllDeviceDigests()
	writeJSON(w, http.StatusOK, map[string]any{
		"upgraded": true,
		"version":  ver,
		"skill":    refreshed,
	})
}

func (s *Server) handleDeleteSkill(w http.ResponseWriter, r *http.Request) {
	deleted, err := s.store.DeleteSkill(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if !deleted {
		writeError(w, http.StatusNotFound, "SKILL_NOT_FOUND", "Skill not found")
		return
	}
	s.engine.InvalidateAllDeviceDigests()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := s.store.ListDevices(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	for i := range devices {
		if hotMs, ok := s.engine.GetDeviceLastSeenMs(devices[i].ID); ok {
			hotISO := time.UnixMilli(hotMs).UTC().Format("2006-01-02T15:04:05.000Z")
			if hotISO > devices[i].LastSeenAt {
				devices[i].LastSeenAt = hotISO
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devices})
}

func (s *Server) handleDeleteDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	deleted, err := s.store.DeleteDevice(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if !deleted {
		writeError(w, http.StatusNotFound, "DEVICE_NOT_FOUND", "Device not found")
		return
	}
	s.engine.InvalidateDeviceDigest(id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeviceReport(w http.ResponseWriter, r *http.Request) {
	rawBytes, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", "Failed to read body")
		return
	}
	var body struct {
		Device *struct {
			ID       string  `json:"id"`
			Name     string  `json:"name"`
			OS       string  `json:"os"`
			Arch     string  `json:"arch"`
			Hostname *string `json:"hostname"`
		} `json:"device"`
		DetectedAgents []store.DetectedAgent     `json:"detectedAgents"`
		ScannedSkills  []store.ScannedSkillInput `json:"scannedSkills"`
	}
	if err := json.Unmarshal(rawBytes, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", "Invalid JSON request body")
		return
	}
	if body.Device == nil || strings.TrimSpace(body.Device.ID) == "" || strings.TrimSpace(body.Device.Name) == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "device.id and device.name are required")
		return
	}

	devID := body.Device.ID
	osName := body.Device.OS
	if osName == "" {
		osName = "unknown"
	}
	arch := body.Device.Arch
	if arch == "" {
		arch = "unknown"
	}

	digest := sha256.Sum256(rawBytes)
	nowMs := time.Now().UnixMilli()

	// Fast memory hot-path: if identical report for an existing device, skip synchronous WriteDB tx
	// and enqueue an async heartbeat update into the 8192 channel.
	if s.engine.RecordDeviceReport(devID, digest, nowMs) {
		existingDev, err := s.store.GetDevice(r.Context(), devID)
		if err == nil && existingDev != nil {
			hotISO := time.UnixMilli(nowMs).UTC().Format("2006-01-02T15:04:05.000Z")
			existingDev.LastSeenAt = hotISO
			s.writer.Enqueue(asyncwrite.WriteEvent{
				Kind:      asyncwrite.EventDeviceHeartbeat,
				DeviceID:  devID,
				CreatedAt: hotISO,
			})
			pendingCount, err := s.store.CountPendingActions(r.Context(), devID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"ok":           true,
				"device":       existingDev,
				"pendingCount": pendingCount,
				"fastPath":     true,
			})
			return
		}
	}

	device, err := s.store.UpsertDevice(
		r.Context(),
		devID,
		body.Device.Name,
		osName,
		arch,
		body.Device.Hostname,
		body.DetectedAgents,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if err := s.store.ReplaceDeviceInstallations(r.Context(), device.ID, body.ScannedSkills); err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	pendingCount, err := s.store.CountPendingActions(r.Context(), device.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	s.engine.UpdateDeviceLastSeen(device.ID, nowMs)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"device":       device,
		"pendingCount": pendingCount,
		"fastPath":     false,
	})
}

func (s *Server) handleClaimActions(w http.ResponseWriter, r *http.Request) {
	devID := r.PathValue("id")
	dev, err := s.store.GetDevice(r.Context(), devID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if dev == nil {
		writeError(w, http.StatusNotFound, "DEVICE_NOT_FOUND", "Device not found")
		return
	}
	actions, err := s.store.ClaimPendingActions(r.Context(), devID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	s.engine.UpdateDeviceLastSeen(devID, time.Now().UnixMilli())
	writeJSON(w, http.StatusOK, map[string]any{"actions": actions})
}

func (s *Server) handleCompleteAction(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Status       string  `json:"status"`
		ErrorMessage *string `json:"errorMessage"`
	}
	if err := readJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	status := skill.ActionStatusFailed
	if body.Status == skill.ActionStatusCompleted {
		status = skill.ActionStatusCompleted
	}
	action, err := s.store.CompleteAction(r.Context(), r.PathValue("id"), status, body.ErrorMessage)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if action == nil {
		writeError(w, http.StatusNotFound, "ACTION_NOT_FOUND", "Action not found")
		return
	}
	s.engine.InvalidateDeviceDigest(action.DeviceID)
	writeJSON(w, http.StatusOK, map[string]any{"action": action})
}

func (s *Server) handleInstallationDiff(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	deviceID := q.Get("deviceId")
	agentID := q.Get("agentId")
	skillName := q.Get("skillName")
	if deviceID == "" || agentID == "" || skillName == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "deviceId, agentId, and skillName are required")
		return
	}

	inst, err := s.store.GetInstallation(r.Context(), deviceID, agentID, skillName, true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if inst == nil {
		writeError(w, http.StatusNotFound, "INSTALLATION_NOT_FOUND", "Installation not found")
		return
	}

	var targetVersion *store.SkillVersion
	if inst.VaultSkillID != nil && *inst.VaultSkillID != "" {
		sk, err := s.store.GetSkill(r.Context(), *inst.VaultSkillID, false, true)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
			return
		}
		if sk != nil {
			targetVersion = sk.CurrentVersion
		}
	}

	var baseVersion *store.SkillVersion
	if inst.VaultSkillID != nil && *inst.VaultSkillID != "" && inst.BaseHash != nil && *inst.BaseHash != "" {
		baseVersion, err = s.store.FindVersionByHash(r.Context(), *inst.VaultSkillID, *inst.BaseHash)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
			return
		}
	}

	omitted := omitActualFiles(inst)

	switch inst.Status {
	case skill.InstallStatusModified:
		localFiles := nonNilFiles(inst.ActualFiles)
		var targetFiles map[string]skill.FileEntry
		var targetVerNum *int
		if targetVersion != nil {
			targetFiles = nonNilFiles(targetVersion.Files)
			targetVerNum = &targetVersion.VersionNum
		} else {
			targetFiles = map[string]skill.FileEntry{}
		}
		diff, err := skill.DiffFileMaps(targetFiles, localFiles)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "DIFF_ERROR", err.Error())
			return
		}
		var baseToLocal *skill.DiffResult
		if baseVersion != nil {
			d, err := skill.DiffFileMaps(baseVersion.Files, localFiles)
			if err == nil {
				baseToLocal = &d
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"mode":             skill.InstallStatusModified,
			"installation":     omitted,
			"localFiles":       localFiles,
			"targetFiles":      targetFiles,
			"targetVersionNum": targetVerNum,
			"diff":             diff,
			"baseToLocalDiff":  baseToLocal,
		})
		return

	case skill.InstallStatusOutdated:
		var localFiles, targetFiles map[string]skill.FileEntry
		var baseVerNum, targetVerNum *int
		if baseVersion != nil {
			localFiles = nonNilFiles(baseVersion.Files)
			baseVerNum = &baseVersion.VersionNum
		} else {
			localFiles = map[string]skill.FileEntry{}
		}
		if targetVersion != nil {
			targetFiles = nonNilFiles(targetVersion.Files)
			targetVerNum = &targetVersion.VersionNum
		} else {
			targetFiles = map[string]skill.FileEntry{}
		}
		diff, err := skill.DiffFileMaps(localFiles, targetFiles)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "DIFF_ERROR", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"mode":             skill.InstallStatusOutdated,
			"installation":     omitted,
			"localFiles":       localFiles,
			"targetFiles":      targetFiles,
			"baseVersionNum":   baseVerNum,
			"targetVersionNum": targetVerNum,
			"diff":             diff,
		})
		return

	case skill.InstallStatusUnmanaged:
		localFiles := nonNilFiles(inst.ActualFiles)
		diff, err := skill.DiffFileMaps(map[string]skill.FileEntry{}, localFiles)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "DIFF_ERROR", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"mode":         skill.InstallStatusUnmanaged,
			"installation": omitted,
			"localFiles":   localFiles,
			"targetFiles":  map[string]skill.FileEntry{},
			"diff":         diff,
		})
		return

	default:
		files := map[string]skill.FileEntry{}
		var targetVerNum *int
		if targetVersion != nil {
			files = nonNilFiles(targetVersion.Files)
			targetVerNum = &targetVersion.VersionNum
		} else if baseVersion != nil {
			files = nonNilFiles(baseVersion.Files)
		} else if inst.ActualFiles != nil {
			files = nonNilFiles(inst.ActualFiles)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"mode":             inst.Status,
			"installation":     omitted,
			"localFiles":       files,
			"targetFiles":      files,
			"targetVersionNum": targetVerNum,
			"diff": skill.DiffResult{
				HasChanges:   false,
				ChangedCount: 0,
				Files:        make([]skill.FileDiff, 0),
			},
		})
	}
}

func (s *Server) handleDeployInstallations(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SkillID   string `json:"skillId"`
		VersionID string `json:"versionId"`
		SkillName string `json:"skillName"`
		Force     bool   `json:"force"`
		DeviceID  string `json:"deviceId"`
		AgentID   string `json:"agentId"`
		Targets   []struct {
			DeviceID  string `json:"deviceId"`
			AgentID   string `json:"agentId"`
			SkillName string `json:"skillName"`
		} `json:"targets"`
	}
	if err := readJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	if body.SkillID == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "skillId is required")
		return
	}
	sk, err := s.store.GetSkill(r.Context(), body.SkillID, false, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if sk == nil || sk.CurrentVersionID == nil {
		writeError(w, http.StatusNotFound, "SKILL_NOT_FOUND", "Vault skill not found")
		return
	}

	targets := body.Targets
	if len(targets) == 0 && body.DeviceID != "" && body.AgentID != "" {
		targets = append(targets, struct {
			DeviceID  string `json:"deviceId"`
			AgentID   string `json:"agentId"`
			SkillName string `json:"skillName"`
		}{DeviceID: body.DeviceID, AgentID: body.AgentID})
	}
	if len(targets) == 0 {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "At least one target (deviceId, agentId) is required")
		return
	}

	verID := body.VersionID
	if verID == "" {
		verID = *sk.CurrentVersionID
	}

	createdActions := make([]store.Action, 0, len(targets))
	for _, tgt := range targets {
		dev, err := s.store.GetDevice(r.Context(), tgt.DeviceID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
			return
		}
		if dev == nil {
			writeError(w, http.StatusNotFound, "DEVICE_NOT_FOUND", fmt.Sprintf("Device not found: %s", tgt.DeviceID))
			return
		}
		skName := tgt.SkillName
		if skName == "" {
			skName = body.SkillName
		}
		if skName == "" {
			skName = sk.ID
		}
		act, err := s.store.CreateAction(r.Context(), dev.ID, tgt.AgentID, skName, skill.ActionInstall, map[string]any{
			"vaultSkillId": sk.ID,
			"versionId":    verID,
			"force":        body.Force,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
			return
		}
		createdActions = append(createdActions, *act)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"actions": createdActions})
}

func (s *Server) handleUpdateInstallation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DeviceID  string `json:"deviceId"`
		AgentID   string `json:"agentId"`
		SkillName string `json:"skillName"`
		VersionID string `json:"versionId"`
		Force     bool   `json:"force"`
	}
	if err := readJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	inst, ok := s.requireInstallation(w, r, body.DeviceID, body.AgentID, body.SkillName, false)
	if !ok {
		return
	}
	if inst.VaultSkillID == nil || *inst.VaultSkillID == "" {
		writeError(w, http.StatusBadRequest, "UNMANAGED_SKILL", "Installation is not linked to a Vault skill")
		return
	}
	if inst.Status == skill.InstallStatusModified && !body.Force {
		writeError(w, http.StatusConflict, "DRIFT_CONFLICT", "Local installation has drifted; use promote or reset instead of safe update")
		return
	}
	sk, err := s.store.GetSkill(r.Context(), *inst.VaultSkillID, false, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if sk == nil || sk.CurrentVersionID == nil {
		writeError(w, http.StatusNotFound, "SKILL_NOT_FOUND", "Vault skill version not found")
		return
	}
	verID := body.VersionID
	if verID == "" {
		verID = *sk.CurrentVersionID
	}
	act, err := s.store.CreateAction(r.Context(), inst.DeviceID, inst.AgentID, inst.SkillName, skill.ActionUpdate, map[string]any{
		"vaultSkillId":     sk.ID,
		"versionId":        verID,
		"expectedBaseHash": inst.BaseHash,
		"force":            false,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"action": act})
}

func (s *Server) handleResetInstallation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DeviceID  string `json:"deviceId"`
		AgentID   string `json:"agentId"`
		SkillName string `json:"skillName"`
		VersionID string `json:"versionId"`
	}
	if err := readJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	inst, ok := s.requireInstallation(w, r, body.DeviceID, body.AgentID, body.SkillName, false)
	if !ok {
		return
	}
	if inst.VaultSkillID == nil || *inst.VaultSkillID == "" {
		writeError(w, http.StatusBadRequest, "UNMANAGED_SKILL", "Installation is not linked to a Vault skill")
		return
	}
	sk, err := s.store.GetSkill(r.Context(), *inst.VaultSkillID, false, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if sk == nil || sk.CurrentVersionID == nil {
		writeError(w, http.StatusNotFound, "SKILL_NOT_FOUND", "Vault skill version not found")
		return
	}
	verID := body.VersionID
	if verID == "" {
		verID = *sk.CurrentVersionID
	}
	act, err := s.store.CreateAction(r.Context(), inst.DeviceID, inst.AgentID, inst.SkillName, skill.ActionReset, map[string]any{
		"vaultSkillId": sk.ID,
		"versionId":    verID,
		"force":        true,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"action": act})
}

func (s *Server) handlePromoteInstallation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DeviceID  string `json:"deviceId"`
		AgentID   string `json:"agentId"`
		SkillName string `json:"skillName"`
		Note      string `json:"note"`
	}
	if err := readJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	inst, ok := s.requireInstallation(w, r, body.DeviceID, body.AgentID, body.SkillName, true)
	if !ok {
		return
	}
	if inst.ActualFiles == nil || inst.ActualHash == nil {
		writeError(w, http.StatusBadRequest, "NO_LOCAL_FILES", "No local snapshot available to promote")
		return
	}
	if inst.VaultSkillID == nil || *inst.VaultSkillID == "" {
		writeError(w, http.StatusBadRequest, "UNMANAGED_SKILL", "Unmanaged skill must be adopted into Vault before promoting")
		return
	}

	files, err := skill.NormalizeFileMap(inst.ActualFiles)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	contentHash, err := skill.ComputeFilesHash(files)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	devLabel := inst.DeviceID
	if inst.DeviceName != nil && *inst.DeviceName != "" {
		devLabel = *inst.DeviceName
	}
	note := strings.TrimSpace(body.Note)
	if note == "" {
		note = fmt.Sprintf("Promoted from %s (%s)", devLabel, inst.AgentID)
	}

	ver, err := s.store.AddSkillVersion(r.Context(), *inst.VaultSkillID, files, contentHash, nil, note)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if _, err := s.store.UpdateInstallation(r.Context(), inst.DeviceID, inst.AgentID, inst.SkillName, store.InstallationPatch{
		SetDesiredVersionID: true,
		DesiredVersionID:    &ver.ID,
		SetBaseHash:         true,
		BaseHash:            &contentHash,
		SetStatus:           true,
		Status:              skill.InstallStatusSynced,
		ClearActualFiles:    true,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	act, err := s.store.CreateAction(r.Context(), inst.DeviceID, inst.AgentID, inst.SkillName, skill.ActionAdoptConfirm, map[string]any{
		"vaultSkillId": *inst.VaultSkillID,
		"versionId":    ver.ID,
		"expectedHash": contentHash,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	sk, err := s.store.GetSkill(r.Context(), *inst.VaultSkillID, true, true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	s.engine.InvalidateAllDeviceDigests()
	writeJSON(w, http.StatusCreated, map[string]any{"skill": sk, "version": ver, "action": act})
}

func (s *Server) handleAdoptInstallation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DeviceID          string  `json:"deviceId"`
		AgentID           string  `json:"agentId"`
		SkillName         string  `json:"skillName"`
		TargetSkillID     string  `json:"targetSkillId"`
		PromoteNewVersion bool    `json:"promoteNewVersion"`
		Name              string  `json:"name"`
		Description       *string `json:"description"`
		ID                string  `json:"id"`
		Note              string  `json:"note"`
	}
	if err := readJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	inst, ok := s.requireInstallation(w, r, body.DeviceID, body.AgentID, body.SkillName, true)
	if !ok {
		return
	}
	if inst.ActualFiles == nil || inst.ActualHash == nil {
		writeError(w, http.StatusBadRequest, "NO_LOCAL_FILES", "No local files found to adopt")
		return
	}

	files, err := skill.NormalizeFileMap(inst.ActualFiles)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	contentHash, err := skill.ComputeFilesHash(files)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}

	devLabel := inst.DeviceID
	if inst.DeviceName != nil && *inst.DeviceName != "" {
		devLabel = *inst.DeviceName
	}

	var sk *store.Skill
	var ver *store.SkillVersion
	if body.TargetSkillID != "" {
		sk, err = s.store.GetSkill(r.Context(), body.TargetSkillID, true, true)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
			return
		}
		if sk == nil {
			writeError(w, http.StatusNotFound, "SKILL_NOT_FOUND", "Target Vault skill not found")
			return
		}
		if body.PromoteNewVersion && (sk.CurrentContentHash == nil || *sk.CurrentContentHash != contentHash) {
			note := strings.TrimSpace(body.Note)
			if note == "" {
				note = fmt.Sprintf("Adopted from %s", devLabel)
			}
			ver, err = s.store.AddSkillVersion(r.Context(), sk.ID, files, contentHash, nil, note)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
				return
			}
			sk, err = s.store.GetSkill(r.Context(), sk.ID, true, true)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
				return
			}
		} else {
			ver = sk.CurrentVersion
		}
	} else {
		var meta skill.SkillMeta
		if md, hasMD := files["SKILL.md"]; hasMD && md.Encoding == "utf8" {
			meta = skill.ParseSkillFrontmatter(md.Content)
		}
		name := strings.TrimSpace(body.Name)
		if name == "" {
			name = strings.TrimSpace(meta.Name)
		}
		if name == "" {
			name = inst.SkillName
		}
		desc := meta.Description
		if body.Description != nil {
			desc = *body.Description
		}
		desc = strings.TrimSpace(desc)

		id := ""
		if strings.TrimSpace(body.ID) != "" {
			id = skill.SlugifySkillID(body.ID)
		} else {
			id, err = s.allocateUniqueSkillID(r.Context(), inst.SkillName)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
				return
			}
		}

		sk, err = s.store.CreateSkillWithVersion(r.Context(), store.CreateSkillInput{
			ID:          id,
			Name:        name,
			Description: desc,
			SourceType:  "adopted",
			Note:        fmt.Sprintf("Adopted from %s (%s)", devLabel, inst.AgentID),
			Files:       files,
			ContentHash: contentHash,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
			return
		}
		ver = sk.CurrentVersion
	}

	nextStatus := skill.InstallStatusOutdated
	if ver != nil && ver.ContentHash == contentHash {
		nextStatus = skill.InstallStatusSynced
	}
	var verIDPtr *string
	if ver != nil {
		verIDPtr = &ver.ID
	}

	if _, err := s.store.UpdateInstallation(r.Context(), inst.DeviceID, inst.AgentID, inst.SkillName, store.InstallationPatch{
		SetVaultSkillID:     true,
		VaultSkillID:        &sk.ID,
		SetDesiredVersionID: true,
		DesiredVersionID:    verIDPtr,
		SetBaseHash:         true,
		BaseHash:            &contentHash,
		SetStatus:           true,
		Status:              nextStatus,
		ClearActualFiles:    nextStatus == skill.InstallStatusSynced,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}

	var verIDAny any
	if ver != nil {
		verIDAny = ver.ID
	}
	act, err := s.store.CreateAction(r.Context(), inst.DeviceID, inst.AgentID, inst.SkillName, skill.ActionAdoptConfirm, map[string]any{
		"vaultSkillId": sk.ID,
		"versionId":    verIDAny,
		"expectedHash": contentHash,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	s.engine.InvalidateAllDeviceDigests()
	writeJSON(w, http.StatusCreated, map[string]any{"skill": sk, "version": ver, "action": act})
}

func (s *Server) handleIgnoreInstallation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DeviceID  string `json:"deviceId"`
		AgentID   string `json:"agentId"`
		SkillName string `json:"skillName"`
		Ignored   bool   `json:"ignored"`
	}
	if err := readJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	inst, ok := s.requireInstallation(w, r, body.DeviceID, body.AgentID, body.SkillName, false)
	if !ok {
		return
	}
	updated, err := s.store.UpdateInstallation(r.Context(), inst.DeviceID, inst.AgentID, inst.SkillName, store.InstallationPatch{
		SetIgnored: true,
		Ignored:    body.Ignored,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"installation": omitActualFiles(updated)})
}

func (s *Server) handleRemoveInstallation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DeviceID   string `json:"deviceId"`
		AgentID    string `json:"agentId"`
		SkillName  string `json:"skillName"`
		ForgetOnly bool   `json:"forgetOnly"`
	}
	if err := readJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	inst, ok := s.requireInstallation(w, r, body.DeviceID, body.AgentID, body.SkillName, false)
	if !ok {
		return
	}
	actionType := skill.ActionRemove
	if inst.Status == skill.InstallStatusMissing || body.ForgetOnly {
		actionType = skill.ActionForgetState
	}
	act, err := s.store.CreateAction(r.Context(), inst.DeviceID, inst.AgentID, inst.SkillName, actionType, map[string]any{})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"action": act})
}

func (s *Server) handleListActions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	actions, err := s.store.ListActions(r.Context(), q.Get("deviceId"), q.Get("status"), 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"actions": actions})
}

func (s *Server) handleCancelAction(w http.ResponseWriter, r *http.Request) {
	act, err := s.store.CancelAction(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if act == nil {
		writeError(w, http.StatusNotFound, "ACTION_NOT_FOUND", "Action not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"action": act})
}

// --- High-Concurrency Engine & Quota Handlers ---

func (s *Server) handleSystemRuntime(w http.ResponseWriter, r *http.Request) {
	pragmas, err := s.db.InspectPragmas(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	readStats := s.db.ReadDB.Stats()
	writeStats := s.db.WriteDB.Stats()

	writeJSON(w, http.StatusOK, map[string]any{
		"pragmas": pragmas,
		"readPool": map[string]any{
			"maxOpen": readStats.MaxOpenConnections,
			"open":    readStats.OpenConnections,
			"inUse":   readStats.InUse,
			"idle":    readStats.Idle,
		},
		"writePool": map[string]any{
			"maxOpen": writeStats.MaxOpenConnections,
			"open":    writeStats.OpenConnections,
			"inUse":   writeStats.InUse,
			"idle":    writeStats.Idle,
		},
		"writeTxTotal": s.db.WriteTxTotal(),
		"writerStats":  s.writer.Stats(),
		"config":       s.engine.GetConfig(),
		"routes":       s.engine.RouteSnapshots(),
		"accounts":     s.engine.ListAccountViews(),
	})
}

func (s *Server) handleGetSystemConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"config": s.engine.GetConfig()})
}

func (s *Server) handlePutSystemConfig(w http.ResponseWriter, r *http.Request) {
	var body hotstate.SystemDynamicConfig
	if err := readJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	updated := s.engine.UpdateSystemConfig(body)
	saved, err := s.store.SaveSystemConfig(r.Context(), updated)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"config": saved})
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"accounts": s.engine.ListAccountViews()})
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name           string  `json:"name"`
		Role           string  `json:"role"`
		UnlimitedQuota bool    `json:"unlimitedQuota"`
		BalanceMicros  int64   `json:"balanceMicros"`
		RateLimitRPS   float64 `json:"rateLimitRps"`
		RateLimitBurst float64 `json:"rateLimitBurst"`
	}
	if err := readJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "Account name is required")
		return
	}
	var randBytes [16]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		writeError(w, http.StatusInternalServerError, "CRYPTO_ERROR", err.Error())
		return
	}
	plainToken := "sdk_" + hex.EncodeToString(randBytes[:])
	sum := sha256.Sum256([]byte(plainToken))

	rec, err := s.store.CreateAccount(r.Context(), hotstate.AccountRecord{
		Name:           name,
		TokenHashHex:   hex.EncodeToString(sum[:]),
		TokenPrefix:    plainToken[:10],
		Role:           body.Role,
		Enabled:        true,
		UnlimitedQuota: body.UnlimitedQuota,
		BalanceMicros:  body.BalanceMicros,
		RateLimitRPS:   body.RateLimitRPS,
		RateLimitBurst: body.RateLimitBurst,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if err := s.engine.UpsertAccount(rec); err != nil {
		writeError(w, http.StatusInternalServerError, "ENGINE_ERROR", err.Error())
		return
	}

	var createdView *hotstate.AccountView
	for _, v := range s.engine.ListAccountViews() {
		if v.ID == rec.ID {
			cp := v
			createdView = &cp
			break
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"account": createdView,
		"token":   plainToken,
	})
}

func (s *Server) handleTopUpAccount(w http.ResponseWriter, r *http.Request) {
	acctID := r.PathValue("id")
	var body struct {
		AmountMicros int64 `json:"amountMicros"`
	}
	if err := readJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return
	}
	if body.AmountMicros <= 0 {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "amountMicros must be positive")
		return
	}
	balAfter, err := s.engine.TopUpAccount(acctID, body.AmountMicros)
	if err != nil {
		writeError(w, http.StatusNotFound, "ACCOUNT_NOT_FOUND", err.Error())
		return
	}
	if err := s.store.TopUpAccountDB(r.Context(), acctID, body.AmountMicros, skill.GenerateID("topup"), balAfter); err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	for _, v := range s.engine.ListAccountViews() {
		if v.ID == acctID {
			writeJSON(w, http.StatusOK, map[string]any{"account": v})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "balanceMicros": balAfter})
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	acctID := r.PathValue("id")
	if acctID == "root" {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "Cannot delete root admin account")
		return
	}
	okMem := s.engine.RemoveAccount(acctID)
	okDB, err := s.store.DeleteAccountDB(r.Context(), acctID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if !okMem && !okDB {
		writeError(w, http.StatusNotFound, "ACCOUNT_NOT_FOUND", "Account not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAuditLogs(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("accountId")
	logs, err := s.store.ListAuditLogs(r.Context(), 50)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	ledger, err := s.store.ListQuotaLedger(r.Context(), accountID, 50)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accessLogs":  logs,
		"quotaLedger": ledger,
	})
}

// --- Helpers ---

func (s *Server) checkSingleSkillUpstream(ctx context.Context, sk *store.Skill) (*store.Skill, error) {
	parsed, err := skill.ParseGitHubURL(*sk.UpstreamURL, derefStr(sk.UpstreamRef), derefStr(sk.UpstreamSubpath))
	if err != nil {
		return nil, err
	}
	info, err := s.github.CheckUpstream(ctx, parsed.Owner, parsed.Repo, parsed.Ref, parsed.Subpath, "")
	if err != nil {
		s.engine.RecordUpstreamResult(false, time.Now())
		return nil, err
	}
	s.engine.RecordUpstreamResult(true, time.Now())
	return s.store.UpdateSkillUpstream(ctx, sk.ID, info.LatestRev, info.LatestMessage, skill.ISONow())
}

func (s *Server) requireInstallation(w http.ResponseWriter, r *http.Request, deviceID, agentID, skillName string, includeFiles bool) (*store.Installation, bool) {
	if deviceID == "" || agentID == "" || skillName == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "deviceId, agentId, and skillName are required")
		return nil, false
	}
	inst, err := s.store.GetInstallation(r.Context(), deviceID, agentID, skillName, includeFiles)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return nil, false
	}
	if inst == nil {
		writeError(w, http.StatusNotFound, "INSTALLATION_NOT_FOUND", "Installation not found")
		return nil, false
	}
	return inst, true
}

func (s *Server) allocateUniqueSkillID(ctx context.Context, name string) (string, error) {
	base := skill.SlugifySkillID(name)
	candidate := base
	suffix := 2
	for {
		existing, err := s.store.GetSkill(ctx, candidate, false, false)
		if err != nil {
			return "", err
		}
		if existing == nil {
			return candidate, nil
		}
		candidate = fmt.Sprintf("%s-%d", base, suffix)
		suffix++
	}
}

func buildFilesInput(files map[string]skill.FileEntry, markdown *string) (map[string]skill.FileEntry, error) {
	if files != nil {
		return skill.NormalizeFileMap(files)
	}
	if markdown != nil {
		return skill.NormalizeFileMap(map[string]skill.FileEntry{
			"SKILL.md": {Encoding: "utf8", Content: *markdown, Mode: "0644"},
		})
	}
	return nil, errors.New("either files map or markdown content is required")
}

func readJSONBody(r *http.Request, dest any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return errors.New("invalid JSON request body")
	}
	return nil
}

func omitActualFiles(inst *store.Installation) store.Installation {
	cp := *inst
	cp.ActualFiles = nil
	return cp
}

func nonNilFiles(m map[string]skill.FileEntry) map[string]skill.FileEntry {
	if m == nil {
		return map[string]skill.FileEntry{}
	}
	return m
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": message,
		"code":  code,
	})
}

func writeUpstreamOrBad(w http.ResponseWriter, err error) {
	if ue, ok := skill.IsUpstreamError(err); ok {
		writeError(w, ue.Status, ue.Code, ue.Message)
		return
	}
	writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
}

func setCORSHeaders(h http.Header) {
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
}

func extractBearerToken(header string) string {
	parts := strings.SplitN(strings.TrimSpace(header), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

func extractClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}

func parseISOMillis(ts string) int64 {
	if ts == "" {
		return 0
	}
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return t.UnixMilli()
	}
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t.UnixMilli()
	}
	return 0
}

func strPtrNonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	cp := s
	return &cp
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}
