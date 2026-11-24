package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"skilld"
	"skilld/internal/asyncwrite"
	"skilld/internal/db"
	"skilld/internal/hotstate"
	"skilld/internal/server"
	"skilld/internal/skill"
	"skilld/internal/store"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("[Skilld] Fatal error: %v", err)
	}
}

func run() error {
	defaultHost := envOrDefault("HOST", "0.0.0.0")
	defaultPort := envIntOrDefault("PORT", 3737)
	defaultDataDir := envOrDefault("DATA_DIR", "./.data")
	defaultReadConns := envIntOrDefault("DB_READ_CONNS", db.DefaultReadConns)
	defaultUpstreamMin := envIntOrDefault("UPSTREAM_CHECK_INTERVAL_MIN", 0)

	hostFlag := flag.String("host", defaultHost, "TCP host to bind")
	portFlag := flag.Int("port", defaultPort, "TCP port to listen on (0 for ephemeral)")
	dataDirFlag := flag.String("data-dir", defaultDataDir, "Directory for SQLite database and generated token")
	tokenFlag := flag.String("token", os.Getenv("SKILLD_TOKEN"), "Root Bearer access token")
	readConnsFlag := flag.Int("read-conns", defaultReadConns, "ReadDB connection pool size (clamped 4..6)")
	githubTokenFlag := flag.String("github-token", os.Getenv("GITHUB_TOKEN"), "Optional GitHub API token")
	upstreamMinFlag := flag.Int("upstream-interval-min", defaultUpstreamMin, "Background upstream check interval in minutes (0 disables)")
	flag.Parse()

	absDataDir, err := filepath.Abs(*dataDirFlag)
	if err != nil {
		return fmt.Errorf("resolve data-dir: %w", err)
	}
	if err := os.MkdirAll(absDataDir, 0o755); err != nil {
		return fmt.Errorf("create data-dir: %w", err)
	}

	token, err := resolveOrCreateToken(*tokenFlag, absDataDir)
	if err != nil {
		return fmt.Errorf("resolve token: %w", err)
	}

	ctx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	dbPath := filepath.Join(absDataDir, "skilld.db")
	splitDB, err := db.Open(ctx, db.Options{
		Path:        dbPath,
		ReadConns:   *readConnsFlag,
		MigrationFS: skilld.MigrationFiles,
	})
	if err != nil {
		return fmt.Errorf("open split sqlite db: %w", err)
	}
	defer func() {
		if closeErr := splitDB.Close(); closeErr != nil {
			log.Printf("[Skilld] Close SplitDB error: %v", closeErr)
		}
	}()

	pragmas, err := splitDB.InspectPragmas(ctx)
	if err != nil {
		return fmt.Errorf("inspect pragmas: %w", err)
	}

	st := store.New(splitDB)
	engine := hotstate.NewEngine()

	sum := sha256.Sum256([]byte(token))
	tokenPrefix := token
	if len(tokenPrefix) > 8 {
		tokenPrefix = tokenPrefix[:8]
	}
	if err := st.EnsureRootAccount(ctx, hex.EncodeToString(sum[:]), tokenPrefix); err != nil {
		return fmt.Errorf("ensure root account: %w", err)
	}
	accounts, err := st.ListAccounts(ctx)
	if err != nil {
		return fmt.Errorf("load accounts: %w", err)
	}
	sysCfg, err := st.LoadSystemConfig(ctx)
	if err != nil {
		return fmt.Errorf("load system config: %w", err)
	}
	if err := engine.LoadFromRecords(sysCfg, accounts); err != nil {
		return fmt.Errorf("hydrate hotstate engine: %w", err)
	}
	engine.UpsertRootToken(token)

	writer := asyncwrite.NewGroupCommitWriter(splitDB)
	ghClient := skill.NewGitHubClient(*githubTokenFlag)

	srv := server.New(server.Options{
		SplitDB:     splitDB,
		Store:       st,
		Engine:      engine,
		Writer:      writer,
		GitHub:      ghClient,
		WebAssets:   skilld.WebAssets,
		GitHubToken: *githubTokenFlag,
	})

	addr := net.JoinHostPort(*hostFlag, strconv.Itoa(*portFlag))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port
	displayHost := *hostFlag
	if displayHost == "0.0.0.0" || displayHost == "::" {
		displayHost = "127.0.0.1"
	}

	httpServer := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	bgCtx, bgCancel := context.WithCancel(ctx)
	defer bgCancel()
	if *upstreamMinFlag > 0 {
		go runUpstreamPoller(bgCtx, *upstreamMinFlag, st, ghClient, engine)
	}

	errCh := make(chan error, 1)
	go func() {
		if serveErr := httpServer.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
			return
		}
		errCh <- nil
	}()

	fmt.Printf("Skilld Server running on http://%s:%d\n", displayHost, actualPort)
	fmt.Printf("Data directory: %s\n", absDataDir)
	fmt.Printf("Database PRAGMAs: journal=%s sync=%d mmap=%d cache=%d wal_autocheckpoint=%d read_query_only=%d read_conns=%d write_conns=%d\n",
		pragmas.WriteJournalMode,
		pragmas.WriteSynchronous,
		pragmas.WriteMmapSize,
		pragmas.WriteCacheSize,
		pragmas.WriteWalAutocheckpoint,
		pragmas.ReadQueryOnly,
		pragmas.ReadMaxOpenConns,
		pragmas.WriteMaxOpenConns,
	)
	fmt.Printf("Async GroupCommit: queue_cap=%d batch_max=%d flush_ms=%d\n",
		asyncwrite.QueueCapacity,
		asyncwrite.BatchMaxSize,
		asyncwrite.FlushInterval.Milliseconds(),
	)
	fmt.Printf("Auth token: %s\n", token)

	select {
	case <-ctx.Done():
		log.Println("[Skilld] Shutdown signal received; draining HTTP connections and async queue...")
	case serveErr := <-errCh:
		if serveErr != nil {
			return serveErr
		}
	}

	bgCancel()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("[Skilld] HTTP shutdown error: %v", err)
	}
	if err := writer.Shutdown(shutdownCtx); err != nil {
		log.Printf("[Skilld] Async writer drain error: %v", err)
	} else {
		stStats := writer.Stats()
		log.Printf("[Skilld] Async queue drained cleanly (flushed_events=%d flushed_batches=%d)",
			stStats.FlushedEvents, stStats.FlushedBatches)
	}

	return nil
}

func runUpstreamPoller(ctx context.Context, intervalMin int, st *store.Store, gh *skill.GitHubClient, engine *hotstate.Engine) {
	ticker := time.NewTicker(time.Duration(intervalMin) * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			skills, err := st.ListSkills(ctx)
			if err != nil {
				continue
			}
			for _, sk := range skills {
				if ctx.Err() != nil {
					return
				}
				if sk.UpstreamURL == nil || *sk.UpstreamURL == "" {
					continue
				}
				parsed, err := skill.ParseGitHubURL(*sk.UpstreamURL, deref(sk.UpstreamRef), deref(sk.UpstreamSubpath))
				if err != nil {
					continue
				}
				info, err := gh.CheckUpstream(ctx, parsed.Owner, parsed.Repo, parsed.Ref, parsed.Subpath, "")
				if err != nil {
					engine.RecordUpstreamResult(false, time.Now())
					continue
				}
				engine.RecordUpstreamResult(true, time.Now())
				_, _ = st.UpdateSkillUpstream(ctx, sk.ID, info.LatestRev, info.LatestMessage, skill.ISONow())
			}
		}
	}
}

func resolveOrCreateToken(explicit, dataDir string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return strings.TrimSpace(explicit), nil
	}
	tokenPath := filepath.Join(dataDir, "token")
	if raw, err := os.ReadFile(tokenPath); err == nil {
		if trimmed := strings.TrimSpace(string(raw)); trimmed != "" {
			return trimmed, nil
		}
	}
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	generated := "sd_" + hex.EncodeToString(b[:])
	if err := os.WriteFile(tokenPath, []byte(generated+"\n"), 0o600); err != nil {
		return "", err
	}
	return generated, nil
}

func envOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envIntOrDefault(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
