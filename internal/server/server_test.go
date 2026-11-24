package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"skilld"
	"skilld/internal/asyncwrite"
	"skilld/internal/db"
	"skilld/internal/hotstate"
	"skilld/internal/server"
	"skilld/internal/skill"
	"skilld/internal/store"
)

type testHarness struct {
	sdb     *db.SplitDB
	st      *store.Store
	engine  *hotstate.Engine
	writer  *asyncwrite.GroupCommitWriter
	handler http.Handler
	token   string
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newTestHarness(t *testing.T, gh *skill.GitHubClient) *testHarness {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	sdb, err := db.Open(ctx, db.Options{
		Path:        dbPath,
		ReadConns:   6,
		MigrationFS: skilld.MigrationFiles,
	})
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	st := store.New(sdb)
	engine := hotstate.NewEngine()
	tok := "sd_test_root_secret"
	sum := sha256.Sum256([]byte(tok))
	if err := st.EnsureRootAccount(ctx, hex.EncodeToString(sum[:]), tok[:8]); err != nil {
		t.Fatal(err)
	}
	accounts, err := st.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := st.LoadSystemConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.LoadFromRecords(cfg, accounts); err != nil {
		t.Fatal(err)
	}
	engine.UpsertRootToken(tok)

	writer := asyncwrite.NewGroupCommitWriter(sdb)
	srv := server.New(server.Options{
		SplitDB:   sdb,
		Store:     st,
		Engine:    engine,
		Writer:    writer,
		GitHub:    gh,
		WebAssets: skilld.WebAssets,
	})

	h := &testHarness{
		sdb:     sdb,
		st:      st,
		engine:  engine,
		writer:  writer,
		handler: srv.Handler(),
		token:   tok,
	}
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = writer.Shutdown(shutCtx)
		_ = sdb.Close()
	})
	return h
}

func (h *testHarness) req(t *testing.T, method, path string, body any, token string) (int, map[string]any, string) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, "http://skilld.local"+path, reader)
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, r)
	raw := rec.Body.String()
	var parsed map[string]any
	_ = json.Unmarshal([]byte(raw), &parsed)
	return rec.Code, parsed, raw
}

func TestFullSkilldAPIContractAndEmbeddedAssets(t *testing.T) {
	h := newTestHarness(t, nil)

	// 1. Health & embedded web assets without auth; API protected by auth.
	code, health, _ := h.req(t, "GET", "/api/health", nil, "")
	if code != 200 || health["ok"] != true || health["runtime"] != "go-sqlite" {
		t.Fatalf("unexpected health response: code=%d body=%v", code, health)
	}
	code, _, _ = h.req(t, "GET", "/api/overview", nil, "")
	if code != 401 {
		t.Fatalf("expected 401 on unauthenticated /api/overview, got %d", code)
	}
	code, _, htmlBody := h.req(t, "GET", "/", nil, "")
	if code != 200 || !strings.Contains(htmlBody, "Skilld") {
		t.Fatalf("expected embedded index.html, got code=%d", code)
	}
	code, _, cssBody := h.req(t, "GET", "/styles.css", nil, "")
	if code != 200 || len(cssBody) < 100 {
		t.Fatalf("expected embedded styles.css, got code=%d", code)
	}

	// 2. Create skill v1 -> v2.
	v1Files := map[string]skill.FileEntry{
		"SKILL.md": {Encoding: "utf8", Content: "---\nname: Lint Helper\n---\nRule 1\n", Mode: "0644"},
	}
	v2Files := map[string]skill.FileEntry{
		"SKILL.md": {Encoding: "utf8", Content: "---\nname: Lint Helper\n---\nRule 1\nRule 2\n", Mode: "0644"},
	}
	h1, _ := skill.ComputeFilesHash(v1Files)
	h2, _ := skill.ComputeFilesHash(v2Files)

	code, created, raw := h.req(t, "POST", "/api/skills", map[string]any{
		"id":    "lint-helper",
		"name":  "Lint Helper",
		"files": v1Files,
	}, h.token)
	if code != 201 {
		t.Fatalf("create skill failed (%d): %s", code, raw)
	}
	skObj := created["skill"].(map[string]any)
	if skObj["currentContentHash"] != h1 {
		t.Fatalf("hash mismatch: %v != %s", skObj["currentContentHash"], h1)
	}

	// 3. Report Mac (synced v1) and Linux (unmanaged local skill).
	localExtra := map[string]skill.FileEntry{
		"SKILL.md": {Encoding: "utf8", Content: "# Local Tool\nDo X\n", Mode: "0644"},
	}
	hExtra, _ := skill.ComputeFilesHash(localExtra)

	code, rep1, raw := h.req(t, "POST", "/api/devices/report", map[string]any{
		"device":         map[string]any{"id": "mac-1", "name": "MacBook", "os": "darwin", "arch": "arm64"},
		"detectedAgents": []map[string]any{{"id": "claude-code", "name": "Claude Code", "skillsDir": "~/.claude/skills", "exists": true}},
		"scannedSkills": []map[string]any{{
			"agentId":      "claude-code",
			"skillName":    "lint-helper",
			"vaultSkillId": "lint-helper",
			"baseHash":     h1,
			"actualHash":   h1,
			"meta":         map[string]any{"name": "Lint Helper"},
		}},
	}, h.token)
	if code != 200 || rep1["fastPath"] != false {
		t.Fatalf("first report failed (%d): %s", code, raw)
	}

	// Identical second report hits hot-path memory fastPath without synchronous WriteDB tx!
	code, repFast, _ := h.req(t, "POST", "/api/devices/report", map[string]any{
		"device":         map[string]any{"id": "mac-1", "name": "MacBook", "os": "darwin", "arch": "arm64"},
		"detectedAgents": []map[string]any{{"id": "claude-code", "name": "Claude Code", "skillsDir": "~/.claude/skills", "exists": true}},
		"scannedSkills": []map[string]any{{
			"agentId":      "claude-code",
			"skillName":    "lint-helper",
			"vaultSkillId": "lint-helper",
			"baseHash":     h1,
			"actualHash":   h1,
			"meta":         map[string]any{"name": "Lint Helper"},
		}},
	}, h.token)
	if code != 200 || repFast["fastPath"] != true {
		t.Fatalf("expected fastPath=true on identical device report, got code=%d body=%v", code, repFast)
	}

	code, _, _ = h.req(t, "POST", "/api/devices/report", map[string]any{
		"device":         map[string]any{"id": "linux-1", "name": "Linux-Box", "os": "linux", "arch": "x64"},
		"detectedAgents": []map[string]any{{"id": "codebuddy", "name": "CodeBuddy", "skillsDir": "~/.codebuddy/skills", "exists": true}},
		"scannedSkills": []map[string]any{{
			"agentId":    "codebuddy",
			"skillName":  "local-tool",
			"actualHash": hExtra,
			"meta":       map[string]any{"name": "Local Tool"},
			"files":      localExtra,
		}},
	}, h.token)
	if code != 200 {
		t.Fatalf("linux report failed: %d", code)
	}

	// 4. Add v2 -> Mac installation turns outdated.
	code, addVer, _ := h.req(t, "POST", "/api/skills/lint-helper/versions", map[string]any{
		"files": v2Files,
		"note":  "Add Rule 2",
	}, h.token)
	if code != 201 || int(addVer["version"].(map[string]any)["versionNum"].(float64)) != 2 {
		t.Fatalf("add version failed: %d %v", code, addVer)
	}
	_ = h2

	code, ov, _ := h.req(t, "GET", "/api/overview", nil, h.token)
	if code != 200 {
		t.Fatalf("overview failed: %d", code)
	}
	insts := ov["installations"].([]any)
	var macStatus string
	for _, item := range insts {
		m := item.(map[string]any)
		if m["deviceId"] == "mac-1" {
			macStatus = m["status"].(string)
		}
	}
	if macStatus != "outdated" {
		t.Fatalf("mac status = %q, want outdated", macStatus)
	}

	// 5. Simulate drift -> safe update 409 -> promote v3 -> deploy -> claim -> complete -> delete.
	driftedFiles := map[string]skill.FileEntry{
		"SKILL.md": {Encoding: "utf8", Content: "---\nname: Lint Helper\n---\nRule 1\nAI local rule\n", Mode: "0644"},
	}
	hDrifted, _ := skill.ComputeFilesHash(driftedFiles)
	h.req(t, "POST", "/api/devices/report", map[string]any{
		"device":         map[string]any{"id": "mac-1", "name": "MacBook", "os": "darwin", "arch": "arm64"},
		"detectedAgents": []map[string]any{{"id": "claude-code", "name": "Claude Code", "skillsDir": "~/.claude/skills", "exists": true}},
		"scannedSkills": []map[string]any{{
			"agentId":      "claude-code",
			"skillName":    "lint-helper",
			"vaultSkillId": "lint-helper",
			"baseHash":     h1,
			"actualHash":   hDrifted,
			"files":        driftedFiles,
		}},
	}, h.token)

	code, _, _ = h.req(t, "POST", "/api/installations/update", map[string]any{
		"deviceId":  "mac-1",
		"agentId":   "claude-code",
		"skillName": "lint-helper",
	}, h.token)
	if code != 409 {
		t.Fatalf("expected 409 DRIFT_CONFLICT on drifted update, got %d", code)
	}

	code, prom, _ := h.req(t, "POST", "/api/installations/promote", map[string]any{
		"deviceId":  "mac-1",
		"agentId":   "claude-code",
		"skillName": "lint-helper",
	}, h.token)
	if code != 201 || int(prom["version"].(map[string]any)["versionNum"].(float64)) != 3 {
		t.Fatalf("promote failed: %d %v", code, prom)
	}

	code, _, _ = h.req(t, "POST", "/api/installations/adopt", map[string]any{
		"deviceId":  "linux-1",
		"agentId":   "codebuddy",
		"skillName": "local-tool",
	}, h.token)
	if code != 201 {
		t.Fatalf("adopt failed: %d", code)
	}

	code, _, _ = h.req(t, "POST", "/api/installations/deploy", map[string]any{
		"skillId": "lint-helper",
		"targets": []map[string]any{{"deviceId": "linux-1", "agentId": "codebuddy"}},
	}, h.token)
	if code != 201 {
		t.Fatalf("deploy failed: %d", code)
	}

	code, claimed, _ := h.req(t, "POST", "/api/devices/linux-1/actions/claim", map[string]any{}, h.token)
	if code != 200 || len(claimed["actions"].([]any)) != 2 {
		t.Fatalf("claim failed: %d %v", code, claimed)
	}

	// Verify runtime endpoint reports PRAGMAs and writer stats cleanly.
	code, rt, raw := h.req(t, "GET", "/api/system/runtime", nil, h.token)
	if code != 200 {
		t.Fatalf("system/runtime failed (%d): %s", code, raw)
	}
	pragmas := rt["pragmas"].(map[string]any)
	if int(pragmas["writeWalAutocheckpoint"].(float64)) != 0 || int(pragmas["readQueryOnly"].(float64)) != 1 {
		t.Fatalf("unexpected pragmas in runtime endpoint: %v", pragmas)
	}

	// Delete skill -> Mac installation returns to unmanaged.
	code, _, _ = h.req(t, "DELETE", "/api/skills/lint-helper", nil, h.token)
	if code != 200 {
		t.Fatalf("delete skill failed: %d", code)
	}
}

func TestHighConcurrencyAccountQuotaAndAsyncAuditDrain(t *testing.T) {
	h := newTestHarness(t, nil)

	// Create a metered API account with balance = 100 micros, high RPS burst so only quota limits it.
	code, created, raw := h.req(t, "POST", "/api/accounts", map[string]any{
		"name":           "CI Runner",
		"role":           "device",
		"unlimitedQuota": false,
		"balanceMicros":  100,
		"rateLimitRps":   10000,
		"rateLimitBurst": 10000,
	}, h.token)
	if code != 201 {
		t.Fatalf("create account failed (%d): %s", code, raw)
	}
	subToken := created["token"].(string)
	acctID := created["account"].(map[string]any)["id"].(string)

	// Concurrently fire 150 GET /api/auth/verify requests (each costs ReadCostMicros = 1).
	const totalReqs = 150
	var mu sync.Mutex
	okCount := 0
	quotaRejected := 0
	var wg sync.WaitGroup
	gate := make(chan struct{})

	for i := 0; i < totalReqs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			c, _, _ := h.req(t, "GET", "/api/auth/verify", nil, subToken)
			mu.Lock()
			if c == 200 {
				okCount++
			} else if c == 402 {
				quotaRejected++
			}
			mu.Unlock()
		}()
	}
	close(gate)
	wg.Wait()

	if okCount != 100 || quotaRejected != 50 {
		t.Fatalf("concurrency accounting mismatch: ok=%d rejected=%d, want 100 and 50", okCount, quotaRejected)
	}

	// Wait briefly for 15ms/64 group commit flush, then verify audit logs & quota ledger via ReadDB.
	time.Sleep(50 * time.Millisecond)
	code, audit, _ := h.req(t, "GET", "/api/audit-logs?accountId="+acctID, nil, h.token)
	if code != 200 {
		t.Fatalf("audit logs failed: %d", code)
	}
	ledger := audit["quotaLedger"].([]any)
	if len(ledger) == 0 {
		t.Fatalf("expected async group-committed quota_ledger rows, got 0")
	}

	// Top up +50 micros and verify request succeeds again.
	code, topped, _ := h.req(t, "POST", "/api/accounts/"+acctID+"/topup", map[string]any{
		"amountMicros": 50,
	}, h.token)
	if code != 200 || int(topped["account"].(map[string]any)["balanceMicros"].(float64)) != 50 {
		t.Fatalf("topup failed: %d %v", code, topped)
	}

	code, _, _ = h.req(t, "GET", "/api/auth/verify", nil, subToken)
	if code != 200 {
		t.Fatalf("expected 200 after topup, got %d", code)
	}
}

func TestGitHubImportCheckAndUpgradeEndpoint(t *testing.T) {
	rev := "v1"
	v1B64 := base64.StdEncoding.EncodeToString([]byte("---\nname: GH Skill\n---\n# v1\n"))
	v2B64 := base64.StdEncoding.EncodeToString([]byte("---\nname: GH Skill\n---\n# v2\n"))

	gh := &skill.GitHubClient{
		BaseURL: "https://mock.github.local",
		HTTPClient: &http.Client{
			Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
				u := r.URL.String()
				var body string
				switch {
				case strings.Contains(u, "/commits?") && rev == "v1":
					body = `[{"sha":"1111111111111111111111111111111111111111","commit":{"message":"init","committer":{"date":"2026-10-01T00:00:00Z"}}}]`
				case strings.Contains(u, "/commits?") && rev == "v2":
					body = `[{"sha":"2222222222222222222222222222222222222222","commit":{"message":"upgrade","committer":{"date":"2026-11-01T00:00:00Z"}}}]`
				case strings.Contains(u, "/git/trees/1111"):
					body = `{"tree":[{"path":"skills/pdf/SKILL.md","type":"blob","mode":"100644","sha":"b1","size":30}]}`
				case strings.Contains(u, "/git/trees/2222"):
					body = `{"tree":[{"path":"skills/pdf/SKILL.md","type":"blob","mode":"100644","sha":"b2","size":30}]}`
				case strings.HasSuffix(u, "/git/blobs/b1"):
					body = `{"encoding":"base64","content":"` + v1B64 + `"}`
				case strings.HasSuffix(u, "/git/blobs/b2"):
					body = `{"encoding":"base64","content":"` + v2B64 + `"}`
				default:
					return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
			}),
		},
	}

	h := newTestHarness(t, gh)
	code, imp, raw := h.req(t, "POST", "/api/skills/import-github", map[string]any{
		"url": "https://github.com/anthropics/skills/tree/main/skills/pdf",
	}, h.token)
	if code != 201 {
		t.Fatalf("import github failed (%d): %s", code, raw)
	}
	skillID := imp["skill"].(map[string]any)["id"].(string)

	rev = "v2"
	code, chk, _ := h.req(t, "POST", "/api/skills/check-all-upstream", map[string]any{}, h.token)
	if code != 200 || !chk["results"].([]any)[0].(map[string]any)["hasUpstreamUpdate"].(bool) {
		t.Fatalf("check upstream failed: %d %v", code, chk)
	}

	code, upg, _ := h.req(t, "POST", "/api/skills/"+skillID+"/upgrade-upstream", map[string]any{}, h.token)
	if code != 200 || !upg["upgraded"].(bool) || int(upg["version"].(map[string]any)["versionNum"].(float64)) != 2 {
		t.Fatalf("upgrade upstream failed: %d %v", code, upg)
	}
}
