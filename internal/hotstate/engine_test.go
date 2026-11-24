package hotstate_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"skillvault/internal/hotstate"
)

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func TestEngineConcurrentPreDeductionNoOverspend(t *testing.T) {
	engine := hotstate.NewEngine()
	tok := "svk_concurrent_quota_test"
	err := engine.UpsertAccount(hotstate.AccountRecord{
		ID:             "acct-1",
		Name:           "Worker Account",
		TokenHashHex:   hashToken(tok),
		TokenPrefix:    "svk_conc",
		Role:           "device",
		Enabled:        true,
		UnlimitedQuota: false,
		BalanceMicros:  1000,
		RateLimitRPS:   100000,
		RateLimitBurst: 100000,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("UpsertAccount failed: %v", err)
	}

	const goroutines = 200
	const costPerReq = int64(10)

	var admitted atomic.Int64
	var rejected atomic.Int64
	var wg sync.WaitGroup
	startGate := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startGate
			res, admitErr := engine.AdmitRequest(tok, "POST /api/devices/report", costPerReq, time.Now())
			if admitErr != nil {
				if admitErr.Code == "QUOTA_EXCEEDED" {
					rejected.Add(1)
				} else {
					t.Errorf("unexpected error code: %s (%s)", admitErr.Code, admitErr.Message)
				}
				return
			}
			admitted.Add(1)
			engine.CommitReservation(res, 200, costPerReq)
		}()
	}

	close(startGate)
	wg.Wait()

	if admitted.Load() != 100 {
		t.Fatalf("admitted = %d, want 100", admitted.Load())
	}
	if rejected.Load() != 100 {
		t.Fatalf("rejected = %d, want 100", rejected.Load())
	}

	views := engine.ListAccountViews()
	if len(views) != 1 {
		t.Fatalf("views len = %d, want 1", len(views))
	}
	if views[0].BalanceMicros != 0 {
		t.Fatalf("final balance = %d, want 0 (never negative)", views[0].BalanceMicros)
	}
	if views[0].ReservedMicros != 0 {
		t.Fatalf("final reserved = %d, want 0", views[0].ReservedMicros)
	}
	if views[0].TotalConsumed != 1000 {
		t.Fatalf("totalConsumed = %d, want 1000", views[0].TotalConsumed)
	}
}

func TestEngineRollbackRestoresQuotaAndDynamicRouteSwitch(t *testing.T) {
	engine := hotstate.NewEngine()
	tok := "svk_rollback_test"
	if err := engine.UpsertAccount(hotstate.AccountRecord{
		ID:             "acct-rb",
		Name:           "Rollback Tester",
		TokenHashHex:   hashToken(tok),
		TokenPrefix:    "svk_roll",
		Role:           "device",
		Enabled:        true,
		BalanceMicros:  50,
		RateLimitRPS:   1000,
		RateLimitBurst: 1000,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := engine.AdmitRequest(tok, "POST /api/skills", 20, time.Now())
	if err != nil {
		t.Fatalf("AdmitRequest failed: %v", err)
	}
	if res.Account.Balance() != 30 || res.Account.Reserved() != 20 {
		t.Fatalf("unexpected pre-deduction state: balance=%d reserved=%d", res.Account.Balance(), res.Account.Reserved())
	}

	engine.RollbackReservation(res)
	if res.Account.Balance() != 50 || res.Account.Reserved() != 0 {
		t.Fatalf("rollback failed: balance=%d reserved=%d", res.Account.Balance(), res.Account.Reserved())
	}

	// Dynamic config snapshot flips route off without touching DB.
	cfg := engine.GetConfig()
	cfg.RouteOverrides["POST /api/skills"] = false
	updated := engine.UpdateSystemConfig(cfg)
	if updated.Version <= 1 {
		t.Fatalf("expected config version increment, got %d", updated.Version)
	}

	_, admitErr := engine.AdmitRequest(tok, "POST /api/skills", 20, time.Now())
	if admitErr == nil || admitErr.Status != 503 || admitErr.Code != "ROUTE_DISABLED" {
		t.Fatalf("expected 503 ROUTE_DISABLED, got %+v", admitErr)
	}
}

func TestEngineRateLimiterAndDeviceHeartbeatCache(t *testing.T) {
	engine := hotstate.NewEngine()
	tok := "svk_ratelimit_test"
	if err := engine.UpsertAccount(hotstate.AccountRecord{
		ID:             "acct-rl",
		Name:           "Rate Limited",
		TokenHashHex:   hashToken(tok),
		TokenPrefix:    "svk_rl",
		Role:           "device",
		Enabled:        true,
		UnlimitedQuota: true,
		RateLimitRPS:   1,
		RateLimitBurst: 2,
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	for i := 0; i < 2; i++ {
		res, err := engine.AdmitRequest(tok, "GET /api/overview", 1, now)
		if err != nil {
			t.Fatalf("burst request %d failed: %v", i, err)
		}
		engine.CommitReservation(res, 200, 1)
	}

	_, err := engine.AdmitRequest(tok, "GET /api/overview", 1, now)
	if err == nil || err.Status != 429 || err.Code != "RATE_LIMITED" {
		t.Fatalf("expected 429 RATE_LIMITED after burst exhausted, got %+v", err)
	}

	// Verify device scan digest fast heartbeat detection.
	digest := sha256.Sum256([]byte("scan-v1"))
	if unchanged := engine.RecordDeviceReport("mac-1", digest, 1000); unchanged {
		t.Fatalf("first report should not be marked unchanged")
	}
	if unchanged := engine.RecordDeviceReport("mac-1", digest, 2000); !unchanged {
		t.Fatalf("second identical report should be marked unchanged")
	}
	if ts, ok := engine.GetDeviceLastSeenMs("mac-1"); !ok || ts != 2000 {
		t.Fatalf("expected lastSeenMs=2000, got %d (ok=%v)", ts, ok)
	}
}

func BenchmarkHotPathAdmitCommitParallel(b *testing.B) {
	engine := hotstate.NewEngine()
	tokens := make([]string, 16)
	for i := range tokens {
		tokens[i] = fmt.Sprintf("svk_bench_%02d", i)
		if err := engine.UpsertAccount(hotstate.AccountRecord{
			ID:             fmt.Sprintf("acct-b-%d", i),
			Name:           "Bench",
			TokenHashHex:   hashToken(tokens[i]),
			TokenPrefix:    "svk_bench",
			Role:           "device",
			Enabled:        true,
			UnlimitedQuota: false,
			BalanceMicros:  1_000_000_000,
			RateLimitRPS:   1e9,
			RateLimitBurst: 1e9,
		}); err != nil {
			b.Fatal(err)
		}
	}

	var idx atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			tok := tokens[idx.Add(1)&15]
			res, err := engine.AdmitRequest(tok, "GET /api/overview", 1, time.Now())
			if err != nil {
				b.Fatalf("AdmitRequest failed: %v", err)
			}
			engine.CommitReservation(res, 200, 1)
		}
	})
}
