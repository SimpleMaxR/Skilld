package db_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"skillvault"
	"skillvault/internal/db"
)

func TestSplitDBPragmasReadOnlyAndPoolConstraints(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "skillvault.db")

	sdb, err := db.Open(ctx, db.Options{
		Path:        dbPath,
		ReadConns:   6,
		MigrationFS: skillvault.MigrationFiles,
	})
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer func() {
		if closeErr := sdb.Close(); closeErr != nil {
			t.Fatalf("sdb.Close failed: %v", closeErr)
		}
	}()

	report, err := sdb.InspectPragmas(ctx)
	if err != nil {
		t.Fatalf("InspectPragmas failed: %v", err)
	}

	if !strings.Contains(report.ReadDSN, "mode=ro&_query_only=on") {
		t.Fatalf("ReadDSN missing mode=ro&_query_only=on: %s", report.ReadDSN)
	}
	if !strings.Contains(report.WriteDSN, "_txlock=immediate") {
		t.Fatalf("WriteDSN missing _txlock=immediate: %s", report.WriteDSN)
	}
	if sdb.WriteDB.Stats().MaxOpenConnections != 1 {
		t.Fatalf("WriteDB MaxOpenConnections = %d, want 1", sdb.WriteDB.Stats().MaxOpenConnections)
	}
	if got := sdb.ReadDB.Stats().MaxOpenConnections; got < 4 || got > 6 {
		t.Fatalf("ReadDB MaxOpenConnections = %d, want 4..6", got)
	}
	if !strings.EqualFold(report.WriteJournalMode, "wal") {
		t.Fatalf("WriteJournalMode = %q, want wal", report.WriteJournalMode)
	}
	if report.WriteSynchronous != 1 {
		t.Fatalf("WriteSynchronous = %d, want 1 (NORMAL)", report.WriteSynchronous)
	}
	if report.WriteMmapSize != db.MmapSizeBytes {
		t.Fatalf("WriteMmapSize = %d, want %d", report.WriteMmapSize, db.MmapSizeBytes)
	}
	if report.WriteCacheSize != db.CacheSizeKiB {
		t.Fatalf("WriteCacheSize = %d, want %d", report.WriteCacheSize, db.CacheSizeKiB)
	}
	if report.WriteWalAutocheckpoint != 0 {
		t.Fatalf("WriteWalAutocheckpoint = %d, want 0", report.WriteWalAutocheckpoint)
	}
	if report.ReadQueryOnly != 1 {
		t.Fatalf("ReadQueryOnly = %d, want 1", report.ReadQueryOnly)
	}
	if report.ReadMmapSize != db.MmapSizeBytes || report.ReadCacheSize != db.CacheSizeKiB {
		t.Fatalf("unexpected ReadDB PRAGMAs: mmap=%d cache=%d", report.ReadMmapSize, report.ReadCacheSize)
	}

	// Verify ReadDB rejects writes even on existing migrated tables.
	_, writeErr := sdb.ReadDB.ExecContext(ctx, `INSERT INTO system_configs (key, value_json, version, updated_at) VALUES ('k', '{}', 1, 'now')`)
	if writeErr == nil {
		t.Fatalf("expected ReadDB write attempt to fail, but it succeeded")
	}

	// Verify all expected migrated tables exist via ReadDB.
	rows, err := sdb.ReadDB.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatalf("list tables failed: %v", err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	wantTables := []string{
		"access_logs",
		"actions",
		"api_accounts",
		"devices",
		"installations",
		"quota_ledger",
		"schema_migrations",
		"skill_versions",
		"skills",
		"system_configs",
	}
	if strings.Join(tables, ",") != strings.Join(wantTables, ",") {
		t.Fatalf("migrated tables = %v, want %v", tables, wantTables)
	}
}

func TestClampReadConns(t *testing.T) {
	if got := db.ClampReadConns(0); got != 6 {
		t.Fatalf("ClampReadConns(0) = %d, want 6", got)
	}
	if got := db.ClampReadConns(2); got != 4 {
		t.Fatalf("ClampReadConns(2) = %d, want 4", got)
	}
	if got := db.ClampReadConns(5); got != 5 {
		t.Fatalf("ClampReadConns(5) = %d, want 5", got)
	}
	if got := db.ClampReadConns(16); got != 6 {
		t.Fatalf("ClampReadConns(16) = %d, want 6", got)
	}
}

func TestSplitDBConcurrentReadsAndWritesZeroBusy(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "concurrent.db")

	sdb, err := db.Open(ctx, db.Options{
		Path:        dbPath,
		ReadConns:   6,
		MigrationFS: skillvault.MigrationFiles,
	})
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer sdb.Close()

	const writers = 50
	const readers = 50
	const writesPerWorker = 10

	var wg sync.WaitGroup
	errCh := make(chan error, writers*writesPerWorker+readers*20)

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < writesPerWorker; i++ {
				key := fmt.Sprintf("cfg-%d-%d", workerID, i)
				err := sdb.WithWriteTx(ctx, func(tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx,
						`INSERT INTO system_configs (key, value_json, version, updated_at) VALUES (?, '{"ok":true}', 1, '2026-11-24T00:00:00Z')`,
						key,
					)
					return err
				})
				if err != nil {
					errCh <- fmt.Errorf("writer %d iter %d: %w", workerID, i, err)
				}
			}
		}(w)
	}

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				var cnt int
				if err := sdb.ReadDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM system_configs`).Scan(&cnt); err != nil {
					errCh <- fmt.Errorf("reader %d iter %d: %w", readerID, i, err)
				}
			}
		}(r)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("concurrent error (must be zero SQLITE_BUSY): %v", err)
	}

	var total int
	if err := sdb.ReadDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM system_configs`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != writers*writesPerWorker {
		t.Fatalf("total rows = %d, want %d", total, writers*writesPerWorker)
	}
}
