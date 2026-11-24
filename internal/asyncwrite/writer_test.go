package asyncwrite_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"skillvault"
	"skillvault/internal/asyncwrite"
	"skillvault/internal/db"
)

func TestGroupCommitBatch64Timer15msAndGracefulDrain(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "async.db")

	sdb, err := db.Open(ctx, db.Options{
		Path:        dbPath,
		ReadConns:   4,
		MigrationFS: skillvault.MigrationFiles,
	})
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer sdb.Close()

	// Seed an account row so account balance updates can also be verified.
	_, err = sdb.ExecWrite(ctx, `
		INSERT INTO api_accounts (
			id, name, token_hash, token_prefix, role, enabled, unlimited_quota,
			balance_micros, total_consumed, rate_limit_rps, rate_limit_burst, created_at, updated_at
		) VALUES ('acct-gc', 'GC Tester', 'hash1', 'svk_1', 'device', 1, 0, 50000, 0, 1000, 1000, '2026-11-24T00:00:00Z', '2026-11-24T00:00:00Z')
	`)
	if err != nil {
		t.Fatalf("seed account failed: %v", err)
	}

	writer := asyncwrite.NewGroupCommitWriter(sdb)
	if writer.Capacity() != 8192 {
		t.Fatalf("writer.Capacity() = %d, want 8192", writer.Capacity())
	}

	nowStr := time.Now().UTC().Format(time.RFC3339Nano)

	// 1. Push exactly 64 events -> must flush immediately as 1 batch before 15ms timer elapses.
	for i := 0; i < 64; i++ {
		ok := writer.Enqueue(asyncwrite.WriteEvent{
			Kind:       asyncwrite.EventAccessLog,
			RequestID:  fmt.Sprintf("req-b1-%d", i),
			AccountID:  "acct-gc",
			Method:     "GET",
			Path:       "/api/overview",
			Route:      "GET /api/overview",
			StatusCode: 200,
			LatencyUs:  42,
			CostMicros: 1,
			ClientIP:   "127.0.0.1",
			CreatedAt:  nowStr,
		})
		if !ok {
			t.Fatalf("enqueue failed at %d", i)
		}
	}

	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		st := writer.Stats()
		if st.FlushedEvents >= 64 {
			break
		}
		time.Sleep(1 * time.Millisecond)
	}

	st1 := writer.Stats()
	if st1.FlushedEvents != 64 || st1.FlushedBatches != 1 {
		t.Fatalf("after 64 events: flushedEvents=%d flushedBatches=%d, want 64 and 1", st1.FlushedEvents, st1.FlushedBatches)
	}

	// 2. Push 7 quota events (< 64) -> must flush automatically via 15ms ticker.
	for i := 0; i < 7; i++ {
		ok := writer.Enqueue(asyncwrite.WriteEvent{
			Kind:         asyncwrite.EventQuotaDeduct,
			RequestID:    fmt.Sprintf("req-q-%d", i),
			AccountID:    "acct-gc",
			Route:        "POST /api/skills",
			CostMicros:   10,
			BalanceAfter: int64(50000 - (i+1)*10),
			CreatedAt:    nowStr,
		})
		if !ok {
			t.Fatalf("enqueue quota failed at %d", i)
		}
	}

	deadline = time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		if writer.Stats().FlushedEvents >= 71 {
			break
		}
		time.Sleep(3 * time.Millisecond)
	}
	st2 := writer.Stats()
	if st2.FlushedEvents != 71 || st2.FlushedBatches != 2 {
		t.Fatalf("after 15ms tick: flushedEvents=%d flushedBatches=%d, want 71 and 2", st2.FlushedEvents, st2.FlushedBatches)
	}

	// 3. Burst 3000 events and immediately Shutdown -> Drain must persist 100% with zero data loss.
	const burstCount = 3000
	for i := 0; i < burstCount; i++ {
		ok := writer.Enqueue(asyncwrite.WriteEvent{
			Kind:       asyncwrite.EventAccessLog,
			RequestID:  fmt.Sprintf("req-drain-%d", i),
			AccountID:  "acct-gc",
			Method:     "POST",
			Path:       "/api/devices/report",
			Route:      "POST /api/devices/report",
			StatusCode: 200,
			LatencyUs:  80,
			CostMicros: 5,
			ClientIP:   "127.0.0.1",
			CreatedAt:  nowStr,
		})
		if !ok {
			t.Fatalf("burst enqueue failed at %d", i)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := writer.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("writer.Shutdown failed: %v", err)
	}

	finalStats := writer.Stats()
	if finalStats.QueueLength != 0 {
		t.Fatalf("expected QueueLength == 0 after drain, got %d", finalStats.QueueLength)
	}
	if finalStats.FlushedEvents != 64+7+burstCount {
		t.Fatalf("FlushedEvents = %d, want %d", finalStats.FlushedEvents, 64+7+burstCount)
	}

	var accessRows int
	if err := sdb.ReadDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM access_logs`).Scan(&accessRows); err != nil {
		t.Fatal(err)
	}
	if accessRows != 64+burstCount {
		t.Fatalf("access_logs rows = %d, want %d", accessRows, 64+burstCount)
	}

	var ledgerRows int
	if err := sdb.ReadDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM quota_ledger`).Scan(&ledgerRows); err != nil {
		t.Fatal(err)
	}
	if ledgerRows != 7 {
		t.Fatalf("quota_ledger rows = %d, want 7", ledgerRows)
	}

	var bal, consumed int64
	if err := sdb.ReadDB.QueryRowContext(ctx, `SELECT balance_micros, total_consumed FROM api_accounts WHERE id = 'acct-gc'`).Scan(&bal, &consumed); err != nil {
		t.Fatal(err)
	}
	if bal != 49930 || consumed != 70 {
		t.Fatalf("account balance=%d consumed=%d, want 49930 and 70", bal, consumed)
	}
}

func BenchmarkGroupCommitThroughput(b *testing.B) {
	ctx := context.Background()
	dbPath := filepath.Join(b.TempDir(), "bench.db")

	sdb, err := db.Open(ctx, db.Options{
		Path:        dbPath,
		ReadConns:   6,
		MigrationFS: skillvault.MigrationFiles,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer sdb.Close()

	writer := asyncwrite.NewGroupCommitWriter(sdb)
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		writer.Enqueue(asyncwrite.WriteEvent{
			Kind:       asyncwrite.EventAccessLog,
			RequestID:  "req-bench",
			AccountID:  "root",
			Method:     "GET",
			Path:       "/api/overview",
			Route:      "GET /api/overview",
			StatusCode: 200,
			LatencyUs:  25,
			CostMicros: 1,
			ClientIP:   "127.0.0.1",
			CreatedAt:  nowStr,
		})
	}
	shutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := writer.Shutdown(shutCtx); err != nil {
		b.Fatal(err)
	}
	b.StopTimer()
}
