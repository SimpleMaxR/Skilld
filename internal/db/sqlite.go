package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mattn/go-sqlite3"
)

const (
	// DriverWrite is the custom sql driver name configured for serialized immediate writes.
	DriverWrite = "sqlite3_skilld_write"
	// DriverRead is the custom sql driver name configured for read-only WAL queries.
	DriverRead = "sqlite3_skilld_read"

	DefaultReadConns  = 6
	MinReadConns      = 4
	MaxReadConns      = 6
	MmapSizeBytes     = 134217728
	CacheSizeKiB      = -32000
	WalAutocheckpoint = 0
)

var registerDriversOnce sync.Once

func registerSplitDrivers() {
	registerDriversOnce.Do(func() {
		sql.Register(DriverWrite, &sqlite3.SQLiteDriver{
			ConnectHook: func(conn *sqlite3.SQLiteConn) error {
				return execConnPragmas(conn, []string{
					"PRAGMA journal_mode = WAL;",
					"PRAGMA synchronous = NORMAL;",
					fmt.Sprintf("PRAGMA mmap_size = %d;", MmapSizeBytes),
					fmt.Sprintf("PRAGMA cache_size = %d;", CacheSizeKiB),
					fmt.Sprintf("PRAGMA wal_autocheckpoint = %d;", WalAutocheckpoint),
					"PRAGMA foreign_keys = ON;",
					"PRAGMA busy_timeout = 5000;",
				})
			},
		})

		sql.Register(DriverRead, &sqlite3.SQLiteDriver{
			ConnectHook: func(conn *sqlite3.SQLiteConn) error {
				return execConnPragmas(conn, []string{
					"PRAGMA query_only = ON;",
					"PRAGMA synchronous = NORMAL;",
					fmt.Sprintf("PRAGMA mmap_size = %d;", MmapSizeBytes),
					fmt.Sprintf("PRAGMA cache_size = %d;", CacheSizeKiB),
					"PRAGMA foreign_keys = ON;",
					"PRAGMA busy_timeout = 5000;",
				})
			},
		})
	})
}

func execConnPragmas(conn *sqlite3.SQLiteConn, stmts []string) error {
	for _, stmt := range stmts {
		if _, err := conn.Exec(stmt, []driver.Value{}); err != nil {
			return fmt.Errorf("apply pragma %q: %w", stmt, err)
		}
	}
	return nil
}

// Options configures a split ReadDB / WriteDB SQLite instance.
type Options struct {
	Path        string
	ReadConns   int
	MigrationFS fs.FS
}

// PragmaReport captures live SQLite PRAGMA and pool configuration for diagnostics.
type PragmaReport struct {
	WriteJournalMode       string `json:"writeJournalMode"`
	WriteSynchronous       int    `json:"writeSynchronous"`
	WriteMmapSize          int64  `json:"writeMmapSize"`
	WriteCacheSize         int64  `json:"writeCacheSize"`
	WriteWalAutocheckpoint int    `json:"writeWalAutocheckpoint"`
	WriteForeignKeys       int    `json:"writeForeignKeys"`
	ReadQueryOnly          int    `json:"readQueryOnly"`
	ReadJournalMode        string `json:"readJournalMode"`
	ReadSynchronous        int    `json:"readSynchronous"`
	ReadMmapSize           int64  `json:"readMmapSize"`
	ReadCacheSize          int64  `json:"readCacheSize"`
	ReadMaxOpenConns       int    `json:"readMaxOpenConns"`
	WriteMaxOpenConns      int    `json:"writeMaxOpenConns"`
	ReadDSN                string `json:"readDsn"`
	WriteDSN               string `json:"writeDsn"`
}

// SplitDB isolates concurrent read-only queries (ReadDB) from Go-mutex-serialized immediate writes (WriteDB).
type SplitDB struct {
	ReadDB    *sql.DB
	WriteDB   *sql.DB
	writeMu   sync.Mutex
	path      string
	readDSN   string
	writeDSN  string
	readConns int

	writeTxTotal atomic.Uint64
	readWaitNs   atomic.Uint64
	writeWaitNs  atomic.Uint64
}

// ClampReadConns enforces the 4..6 concurrent read connection budget.
func ClampReadConns(requested int) int {
	if requested <= 0 {
		return DefaultReadConns
	}
	if requested < MinReadConns {
		return MinReadConns
	}
	if requested > MaxReadConns {
		return MaxReadConns
	}
	return requested
}

// BuildWriteDSN constructs the WriteDB URI enforcing immediate write transactions.
func BuildWriteDSN(dbPath string) (string, error) {
	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		return "", fmt.Errorf("resolve absolute write db path: %w", err)
	}
	u := url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(absPath),
		RawQuery: "_txlock=immediate&_foreign_keys=on&_busy_timeout=5000",
	}
	return u.String(), nil
}

// BuildReadDSN constructs the ReadDB URI enforcing mode=ro and _query_only=on.
func BuildReadDSN(dbPath string) (string, error) {
	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		return "", fmt.Errorf("resolve absolute read db path: %w", err)
	}
	u := url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(absPath),
		RawQuery: "mode=ro&_query_only=on&_foreign_keys=on&_busy_timeout=5000",
	}
	return u.String(), nil
}

// Open initializes WriteDB first (enabling WAL and wal_autocheckpoint=0, running migrations),
// and then opens ReadDB with 4..6 concurrent read-only connections.
func Open(ctx context.Context, opts Options) (*SplitDB, error) {
	if strings.TrimSpace(opts.Path) == "" {
		return nil, errors.New("database path is required")
	}

	absPath, err := filepath.Abs(opts.Path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return nil, fmt.Errorf("create database parent directory: %w", err)
	}

	registerSplitDrivers()

	writeDSN, err := BuildWriteDSN(absPath)
	if err != nil {
		return nil, err
	}
	readDSN, err := BuildReadDSN(absPath)
	if err != nil {
		return nil, err
	}

	readConns := ClampReadConns(opts.ReadConns)

	// Step 1: Open WriteDB first so the database file and WAL/SHM headers are initialized before ReadDB opens mode=ro.
	writeDB, err := sql.Open(DriverWrite, writeDSN)
	if err != nil {
		return nil, fmt.Errorf("open write db: %w", err)
	}
	writeDB.SetMaxOpenConns(1)
	writeDB.SetMaxIdleConns(1)
	writeDB.SetConnMaxLifetime(0)
	writeDB.SetConnMaxIdleTime(0)

	s := &SplitDB{
		WriteDB:   writeDB,
		path:      absPath,
		readDSN:   readDSN,
		writeDSN:  writeDSN,
		readConns: readConns,
	}

	if err := s.initWriteDB(ctx); err != nil {
		_ = writeDB.Close()
		return nil, err
	}

	if opts.MigrationFS != nil {
		if err := s.RunMigrations(ctx, opts.MigrationFS); err != nil {
			_ = writeDB.Close()
			return nil, fmt.Errorf("run migrations: %w", err)
		}
	}

	// Step 2: Open ReadDB pool with mode=ro&_query_only=on and 4..6 connections.
	readDB, err := sql.Open(DriverRead, readDSN)
	if err != nil {
		_ = writeDB.Close()
		return nil, fmt.Errorf("open read db: %w", err)
	}
	readDB.SetMaxOpenConns(readConns)
	readDB.SetMaxIdleConns(readConns)
	readDB.SetConnMaxLifetime(0)
	readDB.SetConnMaxIdleTime(0)

	if err := readDB.PingContext(ctx); err != nil {
		_ = readDB.Close()
		_ = writeDB.Close()
		return nil, fmt.Errorf("ping read db: %w", err)
	}

	s.ReadDB = readDB
	return s, nil
}

func (s *SplitDB) initWriteDB(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if err := s.WriteDB.PingContext(ctx); err != nil {
		return fmt.Errorf("ping write db: %w", err)
	}

	// Explicitly assert WAL and wal_autocheckpoint=0 on the initialized file so Litestream owns all checkpoints.
	initPragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA synchronous = NORMAL;",
		fmt.Sprintf("PRAGMA mmap_size = %d;", MmapSizeBytes),
		fmt.Sprintf("PRAGMA cache_size = %d;", CacheSizeKiB),
		fmt.Sprintf("PRAGMA wal_autocheckpoint = %d;", WalAutocheckpoint),
		"PRAGMA foreign_keys = ON;",
	}
	for _, stmt := range initPragmas {
		if _, err := s.WriteDB.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("initialize write pragma %q: %w", stmt, err)
		}
	}
	return nil
}

// RunMigrations executes embedded SQL migration scripts sequentially inside write transactions.
func (s *SplitDB) RunMigrations(ctx context.Context, migrationFS fs.FS) error {
	files, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("glob migrations: %w", err)
	}
	if len(files) == 0 {
		files, err = fs.Glob(migrationFS, "*.sql")
		if err != nil {
			return fmt.Errorf("glob root migrations: %w", err)
		}
	}
	sort.Strings(files)

	return s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS schema_migrations (
				filename TEXT PRIMARY KEY,
				applied_at TEXT NOT NULL
			);
		`); err != nil {
			return fmt.Errorf("ensure schema_migrations table: %w", err)
		}

		for _, file := range files {
			baseName := filepath.Base(file)
			var count int
			if err := tx.QueryRowContext(
				ctx,
				`SELECT COUNT(*) FROM schema_migrations WHERE filename = ?`,
				baseName,
			).Scan(&count); err != nil {
				return fmt.Errorf("check migration %s: %w", baseName, err)
			}
			if count > 0 {
				continue
			}

			rawSQL, err := fs.ReadFile(migrationFS, file)
			if err != nil {
				return fmt.Errorf("read migration %s: %w", baseName, err)
			}
			if _, err := tx.ExecContext(ctx, string(rawSQL)); err != nil {
				return fmt.Errorf("apply migration %s: %w", baseName, err)
			}
			if _, err := tx.ExecContext(
				ctx,
				`INSERT INTO schema_migrations (filename, applied_at) VALUES (?, ?)`,
				baseName,
				time.Now().UTC().Format(time.RFC3339Nano),
			); err != nil {
				return fmt.Errorf("record migration %s: %w", baseName, err)
			}
		}
		return nil
	})
}

// WithWriteTx serializes write transactions in Go using writeMu and SQLite BEGIN IMMEDIATE.
func (s *SplitDB) WithWriteTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	start := time.Now()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.writeWaitNs.Add(uint64(time.Since(start).Nanoseconds()))

	tx, err := s.WriteDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin write tx: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit write tx: %w", err)
	}
	s.writeTxTotal.Add(1)
	return nil
}

// ExecWrite executes a serialized single-statement write on WriteDB under writeMu.
func (s *SplitDB) ExecWrite(ctx context.Context, query string, args ...any) (sql.Result, error) {
	start := time.Now()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.writeWaitNs.Add(uint64(time.Since(start).Nanoseconds()))

	res, err := s.WriteDB.ExecContext(ctx, query, args...)
	if err == nil {
		s.writeTxTotal.Add(1)
	}
	return res, err
}

// InspectPragmas verifies live PRAGMA settings on both WriteDB and ReadDB pools.
func (s *SplitDB) InspectPragmas(ctx context.Context) (PragmaReport, error) {
	var rep PragmaReport
	rep.ReadMaxOpenConns = s.readConns
	rep.WriteMaxOpenConns = 1
	rep.ReadDSN = s.readDSN
	rep.WriteDSN = s.writeDSN

	s.writeMu.Lock()
	err := func() error {
		if err := s.WriteDB.QueryRowContext(ctx, "PRAGMA journal_mode;").Scan(&rep.WriteJournalMode); err != nil {
			return err
		}
		if err := s.WriteDB.QueryRowContext(ctx, "PRAGMA synchronous;").Scan(&rep.WriteSynchronous); err != nil {
			return err
		}
		if err := s.WriteDB.QueryRowContext(ctx, "PRAGMA mmap_size;").Scan(&rep.WriteMmapSize); err != nil {
			return err
		}
		if err := s.WriteDB.QueryRowContext(ctx, "PRAGMA cache_size;").Scan(&rep.WriteCacheSize); err != nil {
			return err
		}
		if err := s.WriteDB.QueryRowContext(ctx, "PRAGMA wal_autocheckpoint;").Scan(&rep.WriteWalAutocheckpoint); err != nil {
			return err
		}
		if err := s.WriteDB.QueryRowContext(ctx, "PRAGMA foreign_keys;").Scan(&rep.WriteForeignKeys); err != nil {
			return err
		}
		return nil
	}()
	s.writeMu.Unlock()
	if err != nil {
		return rep, fmt.Errorf("inspect write pragmas: %w", err)
	}

	if err := s.ReadDB.QueryRowContext(ctx, "PRAGMA query_only;").Scan(&rep.ReadQueryOnly); err != nil {
		return rep, fmt.Errorf("inspect read query_only: %w", err)
	}
	if err := s.ReadDB.QueryRowContext(ctx, "PRAGMA journal_mode;").Scan(&rep.ReadJournalMode); err != nil {
		return rep, fmt.Errorf("inspect read journal_mode: %w", err)
	}
	if err := s.ReadDB.QueryRowContext(ctx, "PRAGMA synchronous;").Scan(&rep.ReadSynchronous); err != nil {
		return rep, fmt.Errorf("inspect read synchronous: %w", err)
	}
	if err := s.ReadDB.QueryRowContext(ctx, "PRAGMA mmap_size;").Scan(&rep.ReadMmapSize); err != nil {
		return rep, fmt.Errorf("inspect read mmap_size: %w", err)
	}
	if err := s.ReadDB.QueryRowContext(ctx, "PRAGMA cache_size;").Scan(&rep.ReadCacheSize); err != nil {
		return rep, fmt.Errorf("inspect read cache_size: %w", err)
	}
	return rep, nil
}

// WriteTxTotal returns the number of completed write transactions.
func (s *SplitDB) WriteTxTotal() uint64 {
	return s.writeTxTotal.Load()
}

// Path returns the resolved database file path.
func (s *SplitDB) Path() string {
	return s.path
}

// Close closes ReadDB first and then WriteDB under writeMu serialization.
func (s *SplitDB) Close() error {
	var errs []error
	if s.ReadDB != nil {
		if err := s.ReadDB.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close read db: %w", err))
		}
	}
	s.writeMu.Lock()
	if s.WriteDB != nil {
		if err := s.WriteDB.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close write db: %w", err))
		}
	}
	s.writeMu.Unlock()
	return errors.Join(errs...)
}
