package asyncwrite

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"skilld/internal/db"
)

const (
	// QueueCapacity is the fixed buffered channel capacity for async group commit events.
	QueueCapacity = 8192
	// BatchMaxSize triggers immediate group commit flush once accumulated.
	BatchMaxSize = 64
	// FlushInterval triggers group commit flush at most every 15ms.
	FlushInterval = 15 * time.Millisecond
)

// EventKind identifies the type of asynchronous persistence event.
type EventKind uint8

const (
	EventAccessLog EventKind = iota + 1
	EventQuotaDeduct
	EventDeviceHeartbeat
)

// WriteEvent is queued in memory by request hot paths and batched to SQLite by a single writer goroutine.
type WriteEvent struct {
	Kind         EventKind
	RequestID    string
	AccountID    string
	Unlimited    bool
	Method       string
	Path         string
	Route        string
	StatusCode   int
	LatencyUs    int64
	CostMicros   int64
	BalanceAfter int64
	ClientIP     string
	DeviceID     string
	CreatedAt    string
}

// WriterStats exposes real-time telemetry of the async group-commit pipeline.
type WriterStats struct {
	QueueCapacity   int    `json:"queueCapacity"`
	QueueLength     int    `json:"queueLength"`
	BatchMaxSize    int    `json:"batchMaxSize"`
	FlushIntervalMs int64  `json:"flushIntervalMs"`
	EnqueuedTotal   uint64 `json:"enqueuedTotal"`
	FlushedEvents   uint64 `json:"flushedEvents"`
	FlushedBatches  uint64 `json:"flushedBatches"`
	DroppedTotal    uint64 `json:"droppedTotal"`
	FlushErrors     uint64 `json:"flushErrors"`
}

// GroupCommitWriter consumes a buffered channel (cap 8192) on a single background goroutine,
// coalescing up to 64 events or 15ms intervals into single SQLite transactions.
type GroupCommitWriter struct {
	db            *db.SplitDB
	ch            chan WriteEvent
	batchSize     int
	flushInterval time.Duration

	closeMu sync.RWMutex
	closed  bool
	doneCh  chan struct{}

	enqueuedTotal  atomic.Uint64
	flushedEvents  atomic.Uint64
	flushedBatches atomic.Uint64
	droppedTotal   atomic.Uint64
	flushErrors    atomic.Uint64
}

// NewGroupCommitWriter starts a single background batching writer over SplitDB.
func NewGroupCommitWriter(splitDB *db.SplitDB) *GroupCommitWriter {
	w := &GroupCommitWriter{
		db:            splitDB,
		ch:            make(chan WriteEvent, QueueCapacity),
		batchSize:     BatchMaxSize,
		flushInterval: FlushInterval,
		doneCh:        make(chan struct{}),
	}
	go w.runLoop()
	return w
}

// Capacity returns the channel buffer capacity (8192).
func (w *GroupCommitWriter) Capacity() int {
	return cap(w.ch)
}

// Enqueue pushes a write event into the buffered channel without touching disk.
func (w *GroupCommitWriter) Enqueue(ev WriteEvent) bool {
	w.closeMu.RLock()
	defer w.closeMu.RUnlock()
	if w.closed {
		return false
	}

	select {
	case w.ch <- ev:
		w.enqueuedTotal.Add(1)
		return true
	default:
		if ev.Kind == EventQuotaDeduct {
			timer := time.NewTimer(20 * time.Millisecond)
			defer timer.Stop()
			select {
			case w.ch <- ev:
				w.enqueuedTotal.Add(1)
				return true
			case <-timer.C:
			}
		}
		w.droppedTotal.Add(1)
		return false
	}
}

// Stats returns a thread-safe snapshot of queue depth and flush counters.
func (w *GroupCommitWriter) Stats() WriterStats {
	return WriterStats{
		QueueCapacity:   cap(w.ch),
		QueueLength:     len(w.ch),
		BatchMaxSize:    w.batchSize,
		FlushIntervalMs: w.flushInterval.Milliseconds(),
		EnqueuedTotal:   w.enqueuedTotal.Load(),
		FlushedEvents:   w.flushedEvents.Load(),
		FlushedBatches:  w.flushedBatches.Load(),
		DroppedTotal:    w.droppedTotal.Load(),
		FlushErrors:     w.flushErrors.Load(),
	}
}

// Shutdown closes the event channel and blocks until all queued events are drained to SQLite
// or the provided context expires.
func (w *GroupCommitWriter) Shutdown(ctx context.Context) error {
	w.closeMu.Lock()
	if !w.closed {
		w.closed = true
		close(w.ch)
	}
	w.closeMu.Unlock()

	select {
	case <-w.doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *GroupCommitWriter) runLoop() {
	defer close(w.doneCh)
	ticker := time.NewTicker(w.flushInterval)
	defer ticker.Stop()

	batch := make([]WriteEvent, 0, w.batchSize)
	flushBatch := func() {
		if len(batch) == 0 {
			return
		}
		if err := w.flush(batch); err != nil {
			w.flushErrors.Add(1)
		}
		batch = batch[:0]
	}

	for {
		select {
		case ev, ok := <-w.ch:
			if !ok {
				flushBatch()
				return
			}
			batch = append(batch, ev)
		drainReady:
			for len(batch) < w.batchSize {
				select {
				case ev2, ok2 := <-w.ch:
					if !ok2 {
						flushBatch()
						return
					}
					batch = append(batch, ev2)
				default:
					break drainReady
				}
			}
			if len(batch) >= w.batchSize {
				flushBatch()
			}
		case <-ticker.C:
		drainTick:
			for len(batch) < w.batchSize {
				select {
				case ev, ok := <-w.ch:
					if !ok {
						flushBatch()
						return
					}
					batch = append(batch, ev)
				default:
					break drainTick
				}
			}
			if len(batch) > 0 {
				flushBatch()
			}
		}
	}
}

type accountDelta struct {
	unlimited bool
	consumed  int64
	updatedAt string
}

func (w *GroupCommitWriter) flush(batch []WriteEvent) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := w.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var accessStmt *sql.Stmt
		var ledgerStmt *sql.Stmt
		var hbStmt *sql.Stmt
		var err error

		acctDeltas := make(map[string]*accountDelta)
		hbLatest := make(map[string]string)

		for _, ev := range batch {
			switch ev.Kind {
			case EventAccessLog:
				if accessStmt == nil {
					accessStmt, err = tx.PrepareContext(ctx, `
						INSERT INTO access_logs (
							request_id, account_id, method, path, route, status_code, latency_us, cost_micros, client_ip, created_at
						) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
					if err != nil {
						return fmt.Errorf("prepare access_logs insert: %w", err)
					}
					defer accessStmt.Close()
				}
				if _, err := accessStmt.ExecContext(ctx,
					ev.RequestID,
					ev.AccountID,
					ev.Method,
					ev.Path,
					ev.Route,
					ev.StatusCode,
					ev.LatencyUs,
					ev.CostMicros,
					ev.ClientIP,
					ev.CreatedAt,
				); err != nil {
					return fmt.Errorf("insert access_log: %w", err)
				}

			case EventQuotaDeduct:
				if ledgerStmt == nil {
					ledgerStmt, err = tx.PrepareContext(ctx, `
						INSERT INTO quota_ledger (
							account_id, request_id, route, delta_micros, balance_after, created_at
						) VALUES (?, ?, ?, ?, ?, ?)`)
					if err != nil {
						return fmt.Errorf("prepare quota_ledger insert: %w", err)
					}
					defer ledgerStmt.Close()
				}
				if _, err := ledgerStmt.ExecContext(ctx,
					ev.AccountID,
					ev.RequestID,
					ev.Route,
					-ev.CostMicros,
					ev.BalanceAfter,
					ev.CreatedAt,
				); err != nil {
					return fmt.Errorf("insert quota_ledger: %w", err)
				}

				if ev.AccountID != "" {
					agg, ok := acctDeltas[ev.AccountID]
					if !ok {
						agg = &accountDelta{unlimited: ev.Unlimited}
						acctDeltas[ev.AccountID] = agg
					}
					agg.consumed += ev.CostMicros
					agg.updatedAt = ev.CreatedAt
				}

			case EventDeviceHeartbeat:
				if ev.DeviceID != "" {
					hbLatest[ev.DeviceID] = ev.CreatedAt
				}
			}
		}

		if len(acctDeltas) > 0 {
			for accountID, agg := range acctDeltas {
				if agg.unlimited {
					if _, err := tx.ExecContext(ctx, `
						UPDATE api_accounts
						SET total_consumed = total_consumed + ?, updated_at = ?
						WHERE id = ?`,
						agg.consumed, agg.updatedAt, accountID,
					); err != nil {
						return fmt.Errorf("update unlimited account %s: %w", accountID, err)
					}
				} else {
					if _, err := tx.ExecContext(ctx, `
						UPDATE api_accounts
						SET balance_micros = MAX(0, balance_micros - ?),
						    total_consumed = total_consumed + ?,
						    updated_at = ?
						WHERE id = ?`,
						agg.consumed, agg.consumed, agg.updatedAt, accountID,
					); err != nil {
						return fmt.Errorf("update account balance %s: %w", accountID, err)
					}
				}
			}
		}

		if len(hbLatest) > 0 {
			hbStmt, err = tx.PrepareContext(ctx, `UPDATE devices SET last_seen_at = ? WHERE id = ?`)
			if err != nil {
				return fmt.Errorf("prepare heartbeat update: %w", err)
			}
			defer hbStmt.Close()
			for deviceID, ts := range hbLatest {
				if _, err := hbStmt.ExecContext(ctx, ts, deviceID); err != nil {
					return fmt.Errorf("update device heartbeat %s: %w", deviceID, err)
				}
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	w.flushedBatches.Add(1)
	w.flushedEvents.Add(uint64(len(batch)))
	return nil
}
