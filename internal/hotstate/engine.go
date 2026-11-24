package hotstate

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"hash/fnv"
	"maps"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// SystemDynamicConfig holds dynamically reloadable control-plane parameters stored in ConfigSnapshot.
type SystemDynamicConfig struct {
	Version              int64           `json:"version"`
	GlobalRateLimitRPS   float64         `json:"globalRateLimitRps"`
	GlobalRateLimitBurst float64         `json:"globalRateLimitBurst"`
	ReadCostMicros       int64           `json:"readCostMicros"`
	WriteCostMicros      int64           `json:"writeCostMicros"`
	UpstreamCostMicros   int64           `json:"upstreamCostMicros"`
	RouteOverrides       map[string]bool `json:"routeOverrides"`
}

// DefaultSystemConfig returns sensible high-concurrency defaults.
func DefaultSystemConfig() SystemDynamicConfig {
	return SystemDynamicConfig{
		Version:              1,
		GlobalRateLimitRPS:   500,
		GlobalRateLimitBurst: 1000,
		ReadCostMicros:       1,
		WriteCostMicros:      5,
		UpstreamCostMicros:   20,
		RouteOverrides:       map[string]bool{},
	}
}

// AccountRecord represents persisted or configured API account parameters.
type AccountRecord struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	TokenHashHex   string  `json:"-"`
	TokenPrefix    string  `json:"tokenPrefix"`
	Role           string  `json:"role"`
	Enabled        bool    `json:"enabled"`
	UnlimitedQuota bool    `json:"unlimitedQuota"`
	BalanceMicros  int64   `json:"balanceMicros"`
	TotalConsumed  int64   `json:"totalConsumed"`
	RateLimitRPS   float64 `json:"rateLimitRps"`
	RateLimitBurst float64 `json:"rateLimitBurst"`
	CreatedAt      string  `json:"createdAt"`
	UpdatedAt      string  `json:"updatedAt"`
}

// AccountSlot holds per-account runtime state protected by a fine-grained mutex and atomic counters.
type AccountSlot struct {
	ID             string
	Name           string
	TokenPrefix    string
	Role           string
	UnlimitedQuota bool
	CreatedAt      string
	tokenHash      [32]byte

	mu             sync.Mutex
	rateLimitRPS   float64
	rateLimitBurst float64
	tokens         float64
	lastRefillNs   int64

	enabled            atomic.Bool
	balanceMicros      atomic.Int64
	reservedMicros     atomic.Int64
	totalConsumed      atomic.Int64
	requestCount       atomic.Uint64
	rateLimitedCount   atomic.Uint64
	quotaRejectedCount atomic.Uint64
}

// AccountView is a lock-safe read snapshot of an AccountSlot for API responses.
type AccountView struct {
	ID                 string  `json:"id"`
	Name               string  `json:"name"`
	TokenPrefix        string  `json:"tokenPrefix"`
	Role               string  `json:"role"`
	Enabled            bool    `json:"enabled"`
	UnlimitedQuota     bool    `json:"unlimitedQuota"`
	BalanceMicros      int64   `json:"balanceMicros"`
	ReservedMicros     int64   `json:"reservedMicros"`
	TotalConsumed      int64   `json:"totalConsumed"`
	RateLimitRPS       float64 `json:"rateLimitRps"`
	RateLimitBurst     float64 `json:"rateLimitBurst"`
	RequestCount       uint64  `json:"requestCount"`
	RateLimitedCount   uint64  `json:"rateLimitedCount"`
	QuotaRejectedCount uint64  `json:"quotaRejectedCount"`
	CreatedAt          string  `json:"createdAt"`
}

// Snapshot returns a point-in-time AccountView.
func (a *AccountSlot) Snapshot() AccountView {
	a.mu.Lock()
	name := a.Name
	prefix := a.TokenPrefix
	role := a.Role
	unlimited := a.UnlimitedQuota
	rps := a.rateLimitRPS
	burst := a.rateLimitBurst
	a.mu.Unlock()

	return AccountView{
		ID:                 a.ID,
		Name:               name,
		TokenPrefix:        prefix,
		Role:               role,
		Enabled:            a.enabled.Load(),
		UnlimitedQuota:     unlimited,
		BalanceMicros:      a.balanceMicros.Load(),
		ReservedMicros:     a.reservedMicros.Load(),
		TotalConsumed:      a.totalConsumed.Load(),
		RateLimitRPS:       rps,
		RateLimitBurst:     burst,
		RequestCount:       a.requestCount.Load(),
		RateLimitedCount:   a.rateLimitedCount.Load(),
		QuotaRejectedCount: a.quotaRejectedCount.Load(),
		CreatedAt:          a.CreatedAt,
	}
}

// Balance returns the current unreserved account balance in micros.
func (a *AccountSlot) Balance() int64 {
	return a.balanceMicros.Load()
}

// Reserved returns the currently held pre-deducted quota in micros.
func (a *AccountSlot) Reserved() int64 {
	return a.reservedMicros.Load()
}

// TotalConsumed returns the cumulative committed quota in micros.
func (a *AccountSlot) TotalConsumed() int64 {
	return a.totalConsumed.Load()
}

// ConfigSnapshot is an immutable copy-on-write snapshot read atomically on every request.
type ConfigSnapshot struct {
	Config              SystemDynamicConfig
	AccountsByTokenHash map[[32]byte]*AccountSlot
	AccountsByID        map[string]*AccountSlot
}

// RouteMetrics stores lock-free per-route telemetry and circuit breaker status.
type RouteMetrics struct {
	Route          string
	Enabled        atomic.Bool
	InFlight       atomic.Int64
	TotalRequests  atomic.Uint64
	TotalErrors    atomic.Uint64
	TotalLatencyNs atomic.Uint64
	RateLimited    atomic.Uint64
	QuotaRejected  atomic.Uint64
}

// RouteSnapshot is a serializable view of RouteMetrics.
type RouteSnapshot struct {
	Route         string  `json:"route"`
	Enabled       bool    `json:"enabled"`
	InFlight      int64   `json:"inFlight"`
	TotalRequests uint64  `json:"totalRequests"`
	TotalErrors   uint64  `json:"totalErrors"`
	AvgLatencyMs  float64 `json:"avgLatencyMs"`
	RateLimited   uint64  `json:"rateLimited"`
	QuotaRejected uint64  `json:"quotaRejected"`
}

// Reservation represents an active in-memory quota pre-deduction held for an in-flight request.
type Reservation struct {
	Account        *AccountSlot
	Route          string
	ReservedMicros int64
	StartTime      time.Time
}

// AdmitError represents a fast-path rejection (auth, route disabled, rate limit, or quota).
type AdmitError struct {
	Status  int
	Code    string
	Message string
}

func (e *AdmitError) Error() string {
	return e.Message
}

type ipBucket struct {
	tokens     float64
	lastSeenNs int64
}

type ipRateShard struct {
	mu      sync.Mutex
	buckets map[string]*ipBucket
}

// DeviceHotState caches device lastSeen and scan digest in memory to avoid disk writes on unchanged heartbeats.
type DeviceHotState struct {
	mu           sync.Mutex
	lastSeenMs   atomic.Int64
	scanDigest   [32]byte
	hasDigest    bool
	pendingCount atomic.Int64
}

// Engine owns all lock-free / fine-grained in-memory hot-path state.
type Engine struct {
	snapshot  atomic.Pointer[ConfigSnapshot]
	rebuildMu sync.Mutex

	routes sync.Map // map[string]*RouteMetrics

	ipShards   [64]ipRateShard
	ipRPS      float64
	ipBurst    float64
	ipSweepSeq atomic.Uint64

	devices sync.Map // map[string]*DeviceHotState

	upstreamFailures       atomic.Uint32
	upstreamCircuitUntilNs atomic.Int64
}

// NewEngine initializes the hot-path engine with default config and sharded IP rate limiters.
func NewEngine() *Engine {
	e := &Engine{
		ipRPS:   200,
		ipBurst: 400,
	}
	for i := range e.ipShards {
		e.ipShards[i].buckets = make(map[string]*ipBucket)
	}
	initSnap := &ConfigSnapshot{
		Config:              DefaultSystemConfig(),
		AccountsByTokenHash: make(map[[32]byte]*AccountSlot),
		AccountsByID:        make(map[string]*AccountSlot),
	}
	e.snapshot.Store(initSnap)
	return e
}

// LoadFromRecords initializes or replaces accounts and system config atomically from DB records.
func (e *Engine) LoadFromRecords(cfg SystemDynamicConfig, records []AccountRecord) error {
	e.rebuildMu.Lock()
	defer e.rebuildMu.Unlock()

	cur := e.snapshot.Load()
	byHash := make(map[[32]byte]*AccountSlot, len(records))
	byID := make(map[string]*AccountSlot, len(records))

	for _, rec := range records {
		hashBytes, err := hex.DecodeString(rec.TokenHashHex)
		if err != nil || len(hashBytes) != 32 {
			return errors.New("invalid account token hash")
		}
		var hashArr [32]byte
		copy(hashArr[:], hashBytes)

		var slot *AccountSlot
		if cur != nil {
			if existing, ok := cur.AccountsByID[rec.ID]; ok && existing.tokenHash == hashArr {
				slot = existing
			}
		}
		if slot == nil {
			slot = &AccountSlot{
				ID:             rec.ID,
				Name:           rec.Name,
				TokenPrefix:    rec.TokenPrefix,
				Role:           rec.Role,
				UnlimitedQuota: rec.UnlimitedQuota,
				CreatedAt:      rec.CreatedAt,
				tokenHash:      hashArr,
			}
			slot.balanceMicros.Store(rec.BalanceMicros)
			slot.totalConsumed.Store(rec.TotalConsumed)
		} else {
			slot.mu.Lock()
			slot.Name = rec.Name
			slot.TokenPrefix = rec.TokenPrefix
			slot.Role = rec.Role
			slot.UnlimitedQuota = rec.UnlimitedQuota
			slot.mu.Unlock()
		}
		slot.enabled.Store(rec.Enabled)
		slot.mu.Lock()
		slot.rateLimitRPS = rec.RateLimitRPS
		slot.rateLimitBurst = rec.RateLimitBurst
		if slot.lastRefillNs == 0 && rec.RateLimitBurst > 0 {
			slot.tokens = rec.RateLimitBurst
		}
		slot.mu.Unlock()

		byHash[hashArr] = slot
		byID[rec.ID] = slot
	}

	next := &ConfigSnapshot{
		Config:              cloneSystemConfig(cfg),
		AccountsByTokenHash: byHash,
		AccountsByID:        byID,
	}
	e.snapshot.Store(next)
	return nil
}

// UpsertRootToken ensures the primary admin bearer token is registered in memory with unlimited quota.
func (e *Engine) UpsertRootToken(rawToken string) {
	sum := sha256.Sum256([]byte(rawToken))
	prefix := rawToken
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}

	e.rebuildMu.Lock()
	defer e.rebuildMu.Unlock()

	cur := e.snapshot.Load()
	byHash := maps.Clone(cur.AccountsByTokenHash)
	byID := maps.Clone(cur.AccountsByID)
	if byHash == nil {
		byHash = make(map[[32]byte]*AccountSlot)
	}
	if byID == nil {
		byID = make(map[string]*AccountSlot)
	}

	slot, exists := byID["root"]
	if exists {
		delete(byHash, slot.tokenHash)
	}
	if !exists || slot.tokenHash != sum {
		slot = &AccountSlot{
			ID:             "root",
			Name:           "Vault Root Admin",
			TokenPrefix:    prefix,
			Role:           "admin",
			UnlimitedQuota: true,
			CreatedAt:      time.Now().UTC().Format(time.RFC3339),
			tokenHash:      sum,
			rateLimitRPS:   2000,
			rateLimitBurst: 4000,
			tokens:         4000,
		}
		slot.enabled.Store(true)
	}

	byHash[sum] = slot
	byID["root"] = slot

	e.snapshot.Store(&ConfigSnapshot{
		Config:              cloneSystemConfig(cur.Config),
		AccountsByTokenHash: byHash,
		AccountsByID:        byID,
	})
}

// UpsertAccount inserts or updates a single API account into the copy-on-write snapshot.
func (e *Engine) UpsertAccount(rec AccountRecord) error {
	hashBytes, err := hex.DecodeString(rec.TokenHashHex)
	if err != nil || len(hashBytes) != 32 {
		return errors.New("invalid account token hash")
	}
	var hashArr [32]byte
	copy(hashArr[:], hashBytes)

	e.rebuildMu.Lock()
	defer e.rebuildMu.Unlock()

	cur := e.snapshot.Load()
	byHash := maps.Clone(cur.AccountsByTokenHash)
	byID := maps.Clone(cur.AccountsByID)

	slot, exists := byID[rec.ID]
	if exists && slot.tokenHash == hashArr {
		slot.mu.Lock()
		slot.Name = rec.Name
		slot.TokenPrefix = rec.TokenPrefix
		slot.Role = rec.Role
		slot.UnlimitedQuota = rec.UnlimitedQuota
		slot.rateLimitRPS = rec.RateLimitRPS
		slot.rateLimitBurst = rec.RateLimitBurst
		slot.mu.Unlock()
		slot.enabled.Store(rec.Enabled)
	} else {
		if exists {
			delete(byHash, slot.tokenHash)
		}
		slot = &AccountSlot{
			ID:             rec.ID,
			Name:           rec.Name,
			TokenPrefix:    rec.TokenPrefix,
			Role:           rec.Role,
			UnlimitedQuota: rec.UnlimitedQuota,
			CreatedAt:      rec.CreatedAt,
			tokenHash:      hashArr,
			rateLimitRPS:   rec.RateLimitRPS,
			rateLimitBurst: rec.RateLimitBurst,
			tokens:         rec.RateLimitBurst,
		}
		slot.enabled.Store(rec.Enabled)
		slot.balanceMicros.Store(rec.BalanceMicros)
		slot.totalConsumed.Store(rec.TotalConsumed)
	}

	byHash[hashArr] = slot
	byID[rec.ID] = slot

	e.snapshot.Store(&ConfigSnapshot{
		Config:              cloneSystemConfig(cur.Config),
		AccountsByTokenHash: byHash,
		AccountsByID:        byID,
	})
	return nil
}

// RemoveAccount removes an account from the in-memory lookup snapshot.
func (e *Engine) RemoveAccount(accountID string) bool {
	if accountID == "root" {
		return false
	}
	e.rebuildMu.Lock()
	defer e.rebuildMu.Unlock()

	cur := e.snapshot.Load()
	slot, ok := cur.AccountsByID[accountID]
	if !ok {
		return false
	}
	byHash := maps.Clone(cur.AccountsByTokenHash)
	byID := maps.Clone(cur.AccountsByID)
	delete(byHash, slot.tokenHash)
	delete(byID, accountID)

	e.snapshot.Store(&ConfigSnapshot{
		Config:              cloneSystemConfig(cur.Config),
		AccountsByTokenHash: byHash,
		AccountsByID:        byID,
	})
	return true
}

// TopUpAccount atomically credits an account's in-memory balance under its fine-grained lock.
func (e *Engine) TopUpAccount(accountID string, deltaMicros int64) (int64, error) {
	if deltaMicros <= 0 {
		return 0, errors.New("top-up amount must be positive")
	}
	snap := e.snapshot.Load()
	slot, ok := snap.AccountsByID[accountID]
	if !ok {
		return 0, errors.New("account not found")
	}
	slot.mu.Lock()
	next := slot.balanceMicros.Add(deltaMicros)
	slot.mu.Unlock()
	return next, nil
}

// UpdateSystemConfig swaps the active SystemDynamicConfig atomically via Copy-on-Write.
func (e *Engine) UpdateSystemConfig(cfg SystemDynamicConfig) SystemDynamicConfig {
	e.rebuildMu.Lock()
	defer e.rebuildMu.Unlock()

	cur := e.snapshot.Load()
	nextCfg := cloneSystemConfig(cfg)
	if nextCfg.Version <= cur.Config.Version {
		nextCfg.Version = cur.Config.Version + 1
	}
	if nextCfg.GlobalRateLimitRPS <= 0 {
		nextCfg.GlobalRateLimitRPS = cur.Config.GlobalRateLimitRPS
	}
	if nextCfg.GlobalRateLimitBurst <= 0 {
		nextCfg.GlobalRateLimitBurst = cur.Config.GlobalRateLimitBurst
	}

	e.snapshot.Store(&ConfigSnapshot{
		Config:              nextCfg,
		AccountsByTokenHash: cur.AccountsByTokenHash,
		AccountsByID:        cur.AccountsByID,
	})

	for routeKey, enabled := range nextCfg.RouteOverrides {
		e.getOrCreateRoute(routeKey).Enabled.Store(enabled)
	}
	return cloneSystemConfig(nextCfg)
}

// GetConfig returns a copy of the active SystemDynamicConfig without locking.
func (e *Engine) GetConfig() SystemDynamicConfig {
	return cloneSystemConfig(e.snapshot.Load().Config)
}

// ListAccountViews returns snapshots of all active accounts sorted by ID.
func (e *Engine) ListAccountViews() []AccountView {
	snap := e.snapshot.Load()
	views := make([]AccountView, 0, len(snap.AccountsByID))
	for _, slot := range snap.AccountsByID {
		views = append(views, slot.Snapshot())
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].ID == "root" {
			return true
		}
		if views[j].ID == "root" {
			return false
		}
		return views[i].ID < views[j].ID
	})
	return views
}

// getOrCreateRoute fetches or initializes atomic metrics for a route key.
func (e *Engine) getOrCreateRoute(routeKey string) *RouteMetrics {
	if val, ok := e.routes.Load(routeKey); ok {
		return val.(*RouteMetrics)
	}
	rm := &RouteMetrics{Route: routeKey}
	rm.Enabled.Store(true)
	actual, _ := e.routes.LoadOrStore(routeKey, rm)
	return actual.(*RouteMetrics)
}

// IsRouteEnabled checks dynamic overrides and atomic route toggle in memory.
func (e *Engine) IsRouteEnabled(routeKey string) bool {
	snap := e.snapshot.Load()
	if enabled, overridden := snap.Config.RouteOverrides[routeKey]; overridden {
		return enabled
	}
	if val, ok := e.routes.Load(routeKey); ok {
		return val.(*RouteMetrics).Enabled.Load()
	}
	return true
}

// AdmitRequest executes the zero-disk hot path:
// 1) route circuit check
// 2) constant-time token lookup in atomic ConfigSnapshot
// 3) per-account fine-grained lock rate limiting (token bucket)
// 4) per-account atomic quota pre-deduction
func (e *Engine) AdmitRequest(rawToken string, routeKey string, costMicros int64, now time.Time) (*Reservation, *AdmitError) {
	rm := e.getOrCreateRoute(routeKey)
	if !e.IsRouteEnabled(routeKey) {
		return nil, &AdmitError{
			Status:  503,
			Code:    "ROUTE_DISABLED",
			Message: "Route temporarily disabled by dynamic config",
		}
	}

	if rawToken == "" {
		return nil, &AdmitError{
			Status:  401,
			Code:    "UNAUTHORIZED",
			Message: "Unauthorized: invalid or missing Bearer token",
		}
	}

	snap := e.snapshot.Load()
	sum := sha256.Sum256([]byte(rawToken))
	slot, ok := snap.AccountsByTokenHash[sum]
	if !ok || subtle.ConstantTimeCompare(slot.tokenHash[:], sum[:]) != 1 || !slot.enabled.Load() {
		return nil, &AdmitError{
			Status:  401,
			Code:    "UNAUTHORIZED",
			Message: "Unauthorized: invalid or missing Bearer token",
		}
	}

	nowNs := now.UnixNano()
	slot.mu.Lock()
	rps := slot.rateLimitRPS
	if rps <= 0 {
		rps = snap.Config.GlobalRateLimitRPS
	}
	burst := slot.rateLimitBurst
	if burst <= 0 {
		burst = snap.Config.GlobalRateLimitBurst
	}

	if slot.lastRefillNs == 0 {
		slot.tokens = burst
		slot.lastRefillNs = nowNs
	} else if nowNs > slot.lastRefillNs {
		elapsedSec := float64(nowNs-slot.lastRefillNs) / float64(time.Second)
		slot.tokens += elapsedSec * rps
		if slot.tokens > burst {
			slot.tokens = burst
		}
		slot.lastRefillNs = nowNs
	}

	if slot.tokens < 1.0 {
		slot.mu.Unlock()
		slot.rateLimitedCount.Add(1)
		rm.RateLimited.Add(1)
		return nil, &AdmitError{
			Status:  429,
			Code:    "RATE_LIMITED",
			Message: "Account rate limit exceeded",
		}
	}
	slot.tokens -= 1.0

	reserved := int64(0)
	if !slot.UnlimitedQuota && costMicros > 0 {
		if slot.balanceMicros.Load() < costMicros {
			slot.tokens += 1.0
			slot.mu.Unlock()
			slot.quotaRejectedCount.Add(1)
			rm.QuotaRejected.Add(1)
			return nil, &AdmitError{
				Status:  402,
				Code:    "QUOTA_EXCEEDED",
				Message: "Insufficient account quota balance",
			}
		}
		slot.balanceMicros.Add(-costMicros)
		slot.reservedMicros.Add(costMicros)
		reserved = costMicros
	}

	slot.requestCount.Add(1)
	slot.mu.Unlock()

	rm.InFlight.Add(1)
	return &Reservation{
		Account:        slot,
		Route:          routeKey,
		ReservedMicros: reserved,
		StartTime:      now,
	}, nil
}

// CommitReservation finalizes an in-flight pre-deduction on success / non-5xx responses.
func (e *Engine) CommitReservation(res *Reservation, statusCode int, actualCost int64) (balanceAfter int64, charged int64) {
	if res == nil {
		return 0, 0
	}
	rm := e.getOrCreateRoute(res.Route)
	rm.InFlight.Add(-1)
	rm.TotalRequests.Add(1)
	rm.TotalLatencyNs.Add(uint64(time.Since(res.StartTime).Nanoseconds()))
	if statusCode >= 500 {
		rm.TotalErrors.Add(1)
	}

	slot := res.Account
	if slot == nil {
		return 0, 0
	}

	slot.mu.Lock()
	defer slot.mu.Unlock()

	if slot.UnlimitedQuota {
		if actualCost < 0 {
			actualCost = 0
		}
		slot.totalConsumed.Add(actualCost)
		return slot.balanceMicros.Load(), actualCost
	}

	if res.ReservedMicros <= 0 {
		return slot.balanceMicros.Load(), 0
	}

	if actualCost < 0 {
		actualCost = 0
	}
	if actualCost > res.ReservedMicros {
		actualCost = res.ReservedMicros
	}

	slot.reservedMicros.Add(-res.ReservedMicros)
	refund := res.ReservedMicros - actualCost
	if refund > 0 {
		slot.balanceMicros.Add(refund)
	}
	slot.totalConsumed.Add(actualCost)
	return slot.balanceMicros.Load(), actualCost
}

// RollbackReservation returns pre-deducted quota on internal server failures (5xx).
func (e *Engine) RollbackReservation(res *Reservation) {
	if res == nil {
		return
	}
	rm := e.getOrCreateRoute(res.Route)
	rm.InFlight.Add(-1)
	rm.TotalRequests.Add(1)
	rm.TotalErrors.Add(1)
	rm.TotalLatencyNs.Add(uint64(time.Since(res.StartTime).Nanoseconds()))

	slot := res.Account
	if slot == nil || res.ReservedMicros <= 0 {
		return
	}

	slot.mu.Lock()
	if !slot.UnlimitedQuota {
		slot.reservedMicros.Add(-res.ReservedMicros)
		slot.balanceMicros.Add(res.ReservedMicros)
	}
	slot.mu.Unlock()
}

// AllowUnauthIP applies a 64-way sharded token bucket limiter for public endpoints (/api/health, web assets).
func (e *Engine) AllowUnauthIP(clientIP string, now time.Time) bool {
	if clientIP == "" {
		clientIP = "unknown"
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(clientIP))
	shardIdx := h.Sum32() & 63
	shard := &e.ipShards[shardIdx]

	nowNs := now.UnixNano()
	shard.mu.Lock()
	defer shard.mu.Unlock()

	if e.ipSweepSeq.Add(1)%1024 == 0 && len(shard.buckets) > 256 {
		cutoff := nowNs - int64(5*time.Minute)
		for k, b := range shard.buckets {
			if b.lastSeenNs < cutoff {
				delete(shard.buckets, k)
			}
		}
	}

	b, ok := shard.buckets[clientIP]
	if !ok {
		b = &ipBucket{
			tokens:     e.ipBurst - 1.0,
			lastSeenNs: nowNs,
		}
		shard.buckets[clientIP] = b
		return true
	}

	elapsed := float64(nowNs-b.lastSeenNs) / float64(time.Second)
	if elapsed > 0 {
		b.tokens += elapsed * e.ipRPS
		if b.tokens > e.ipBurst {
			b.tokens = e.ipBurst
		}
	}
	b.lastSeenNs = nowNs

	if b.tokens < 1.0 {
		return false
	}
	b.tokens -= 1.0
	return true
}

// RecordDeviceReport evaluates whether a device report matches the in-memory scan digest.
// When unchanged, only memory state is updated so the caller can enqueue an async heartbeat instead of a sync write tx.
func (e *Engine) RecordDeviceReport(deviceID string, digest [32]byte, nowMs int64) (unchanged bool) {
	val, ok := e.devices.Load(deviceID)
	if !ok {
		st := &DeviceHotState{}
		st.lastSeenMs.Store(nowMs)
		st.scanDigest = digest
		st.hasDigest = true
		actual, loaded := e.devices.LoadOrStore(deviceID, st)
		if !loaded {
			return false
		}
		val = actual
	}

	st := val.(*DeviceHotState)
	st.lastSeenMs.Store(nowMs)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.hasDigest && st.scanDigest == digest {
		return true
	}
	st.scanDigest = digest
	st.hasDigest = true
	return false
}

// InvalidateDeviceDigest clears cached scan digest after actions or mutations affecting the device.
func (e *Engine) InvalidateDeviceDigest(deviceID string) {
	if val, ok := e.devices.Load(deviceID); ok {
		st := val.(*DeviceHotState)
		st.mu.Lock()
		st.hasDigest = false
		st.mu.Unlock()
	}
}

// InvalidateAllDeviceDigests clears cached scan digests across all devices (e.g., when Vault versions change).
func (e *Engine) InvalidateAllDeviceDigests() {
	e.devices.Range(func(_, v any) bool {
		st := v.(*DeviceHotState)
		st.mu.Lock()
		st.hasDigest = false
		st.mu.Unlock()
		return true
	})
}

// UpdateDeviceLastSeen updates the atomic lastSeen timestamp in memory.
func (e *Engine) UpdateDeviceLastSeen(deviceID string, nowMs int64) {
	val, ok := e.devices.Load(deviceID)
	if !ok {
		st := &DeviceHotState{}
		st.lastSeenMs.Store(nowMs)
		actual, _ := e.devices.LoadOrStore(deviceID, st)
		actual.(*DeviceHotState).lastSeenMs.Store(nowMs)
		return
	}
	val.(*DeviceHotState).lastSeenMs.Store(nowMs)
}

// GetDeviceLastSeenMs returns the freshest in-memory lastSeenMs if present.
func (e *Engine) GetDeviceLastSeenMs(deviceID string) (int64, bool) {
	if val, ok := e.devices.Load(deviceID); ok {
		ts := val.(*DeviceHotState).lastSeenMs.Load()
		return ts, ts > 0
	}
	return 0, false
}

// RouteSnapshots returns sorted route telemetry snapshots.
func (e *Engine) RouteSnapshots() []RouteSnapshot {
	snap := e.snapshot.Load()
	out := make([]RouteSnapshot, 0)
	e.routes.Range(func(_, v any) bool {
		rm := v.(*RouteMetrics)
		total := rm.TotalRequests.Load()
		avgMs := 0.0
		if total > 0 {
			avgMs = float64(rm.TotalLatencyNs.Load()) / float64(total) / 1e6
		}
		enabled := rm.Enabled.Load()
		if ov, has := snap.Config.RouteOverrides[rm.Route]; has {
			enabled = ov
		}
		out = append(out, RouteSnapshot{
			Route:         rm.Route,
			Enabled:       enabled,
			InFlight:      rm.InFlight.Load(),
			TotalRequests: total,
			TotalErrors:   rm.TotalErrors.Load(),
			AvgLatencyMs:  avgMs,
			RateLimited:   rm.RateLimited.Load(),
			QuotaRejected: rm.QuotaRejected.Load(),
		})
		return true
	})
	sort.Slice(out, func(i, j int) bool {
		return out[i].Route < out[j].Route
	})
	return out
}

// RecordUpstreamResult updates the atomic GitHub upstream circuit state.
func (e *Engine) RecordUpstreamResult(ok bool, now time.Time) {
	if ok {
		e.upstreamFailures.Store(0)
		e.upstreamCircuitUntilNs.Store(0)
		return
	}
	fails := e.upstreamFailures.Add(1)
	if fails >= 5 {
		e.upstreamCircuitUntilNs.Store(now.Add(30 * time.Second).UnixNano())
	}
}

// UpstreamCircuitOpen returns true if upstream calls are temporarily tripped.
func (e *Engine) UpstreamCircuitOpen(now time.Time) bool {
	until := e.upstreamCircuitUntilNs.Load()
	return until > 0 && now.UnixNano() < until
}

func cloneSystemConfig(cfg SystemDynamicConfig) SystemDynamicConfig {
	out := cfg
	if cfg.RouteOverrides != nil {
		out.RouteOverrides = maps.Clone(cfg.RouteOverrides)
	} else {
		out.RouteOverrides = map[string]bool{}
	}
	return out
}
