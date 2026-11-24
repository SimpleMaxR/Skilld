package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"skilld/internal/db"
	"skilld/internal/hotstate"
	"skilld/internal/skill"
)

var (
	ErrNotFound = errors.New("record not found")
	ErrConflict = errors.New("record conflict")
)

// Store implements Skilld persistence strictly using SplitDB (ReadDB for reads, WithWriteTx on WriteDB for writes).
type Store struct {
	db *db.SplitDB
}

// New constructs a Store backed by a split ReadDB/WriteDB instance.
func New(splitDB *db.SplitDB) *Store {
	return &Store{db: splitDB}
}

// SkillVersion represents a versioned snapshot of a Vault skill.
type SkillVersion struct {
	ID          string                     `json:"id"`
	SkillID     string                     `json:"skillId"`
	VersionNum  int                        `json:"versionNum"`
	ContentHash string                     `json:"contentHash"`
	SourceRev   *string                    `json:"sourceRev"`
	Note        string                     `json:"note"`
	Files       map[string]skill.FileEntry `json:"files,omitempty"`
	CreatedAt   string                     `json:"createdAt"`
}

// Skill represents a Skilld catalog item.
type Skill struct {
	ID                    string         `json:"id"`
	Name                  string         `json:"name"`
	Description           string         `json:"description"`
	SourceType            string         `json:"sourceType"`
	UpstreamURL           *string        `json:"upstreamUrl"`
	UpstreamRef           *string        `json:"upstreamRef"`
	UpstreamSubpath       *string        `json:"upstreamSubpath"`
	UpstreamLatestRev     *string        `json:"upstreamLatestRev"`
	UpstreamLatestMessage *string        `json:"upstreamLatestMessage"`
	UpstreamCheckedAt     *string        `json:"upstreamCheckedAt"`
	CurrentVersionID      *string        `json:"currentVersionId"`
	CurrentVersionNum     *int           `json:"currentVersionNum"`
	CurrentContentHash    *string        `json:"currentContentHash"`
	CurrentSourceRev      *string        `json:"currentSourceRev"`
	HasUpstreamUpdate     bool           `json:"hasUpstreamUpdate"`
	CurrentVersion        *SkillVersion  `json:"currentVersion,omitempty"`
	Versions              []SkillVersion `json:"versions,omitempty"`
	CreatedAt             string         `json:"createdAt"`
	UpdatedAt             string         `json:"updatedAt"`

	InstalledCount int `json:"installedCount,omitempty"`
	OutdatedCount  int `json:"outdatedCount,omitempty"`
	ModifiedCount  int `json:"modifiedCount,omitempty"`
}

// DetectedAgent describes a coding CLI skill directory detected on a device.
type DetectedAgent struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	SkillsDir  string `json:"skillsDir"`
	ConfigRoot string `json:"configRoot,omitempty"`
	Exists     bool   `json:"exists"`
}

// Device represents a registered macOS or Linux development host.
type Device struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	OS             string          `json:"os"`
	Arch           string          `json:"arch"`
	Hostname       *string         `json:"hostname"`
	DetectedAgents []DetectedAgent `json:"detectedAgents"`
	LastSeenAt     string          `json:"lastSeenAt"`
	CreatedAt      string          `json:"createdAt"`
}

// Installation represents a skill state on a (device, agent) pair.
type Installation struct {
	ID                string                     `json:"id"`
	DeviceID          string                     `json:"deviceId"`
	DeviceName        *string                    `json:"deviceName"`
	DeviceOS          *string                    `json:"deviceOs"`
	AgentID           string                     `json:"agentId"`
	SkillName         string                     `json:"skillName"`
	VaultSkillID      *string                    `json:"vaultSkillId"`
	VaultSkillName    *string                    `json:"vaultSkillName"`
	DesiredVersionID  *string                    `json:"desiredVersionId"`
	DesiredVersionNum *int                       `json:"desiredVersionNum"`
	DesiredHash       *string                    `json:"desiredHash"`
	BaseHash          *string                    `json:"baseHash"`
	ActualHash        *string                    `json:"actualHash"`
	Status            string                     `json:"status"`
	Ignored           bool                       `json:"ignored"`
	ActualMeta        *skill.SkillMeta           `json:"actualMeta"`
	HasActualFiles    bool                       `json:"hasActualFiles"`
	ActualFiles       map[string]skill.FileEntry `json:"actualFiles,omitempty"`
	LastScannedAt     string                     `json:"lastScannedAt"`
}

// Action represents a sync operation queued for a device daemon.
type Action struct {
	ID           string         `json:"id"`
	DeviceID     string         `json:"deviceId"`
	AgentID      string         `json:"agentId"`
	SkillName    string         `json:"skillName"`
	Type         string         `json:"type"`
	Payload      map[string]any `json:"payload"`
	Status       string         `json:"status"`
	ErrorMessage *string        `json:"errorMessage"`
	CreatedAt    string         `json:"createdAt"`
	UpdatedAt    string         `json:"updatedAt"`
}

// ScannedSkillInput is the per-skill payload reported by a device daemon.
type ScannedSkillInput struct {
	AgentID      string                     `json:"agentId"`
	SkillName    string                     `json:"skillName"`
	ActualHash   *string                    `json:"actualHash"`
	BaseHash     *string                    `json:"baseHash"`
	VaultSkillID *string                    `json:"vaultSkillId"`
	Meta         *skill.SkillMeta           `json:"meta"`
	Files        map[string]skill.FileEntry `json:"files"`
}

// CreateSkillInput specifies parameters for creating a skill and its v1 snapshot.
type CreateSkillInput struct {
	ID                    string
	Name                  string
	Description           string
	SourceType            string
	UpstreamURL           *string
	UpstreamRef           *string
	UpstreamSubpath       *string
	UpstreamLatestRev     *string
	UpstreamLatestMessage *string
	UpstreamCheckedAt     *string
	SourceRev             *string
	Note                  string
	Files                 map[string]skill.FileEntry
	ContentHash           string
}

// AccessLogRow represents an asynchronously flushed request log entry.
type AccessLogRow struct {
	ID         int64  `json:"id"`
	RequestID  string `json:"requestId"`
	AccountID  string `json:"accountId"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Route      string `json:"route"`
	StatusCode int    `json:"statusCode"`
	LatencyUs  int64  `json:"latencyUs"`
	CostMicros int64  `json:"costMicros"`
	ClientIP   string `json:"clientIp"`
	CreatedAt  string `json:"createdAt"`
}

// QuotaLedgerRow represents an asynchronously flushed quota deduction record.
type QuotaLedgerRow struct {
	ID           int64  `json:"id"`
	AccountID    string `json:"accountId"`
	RequestID    string `json:"requestId"`
	Route        string `json:"route"`
	DeltaMicros  int64  `json:"deltaMicros"`
	BalanceAfter int64  `json:"balanceAfter"`
	CreatedAt    string `json:"createdAt"`
}

type rowScanner interface {
	Scan(dest ...any) error
}

type queryRunner interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func scanSkill(scanner rowScanner) (Skill, error) {
	var (
		s                                          Skill
		upURL, upRef, upSub, upRev, upMsg, upCheck sql.NullString
		curVerID, curHash, curSrcRev               sql.NullString
		curVerNum                                  sql.NullInt64
	)
	err := scanner.Scan(
		&s.ID,
		&s.Name,
		&s.Description,
		&s.SourceType,
		&upURL,
		&upRef,
		&upSub,
		&upRev,
		&upMsg,
		&upCheck,
		&curVerID,
		&s.CreatedAt,
		&s.UpdatedAt,
		&curVerNum,
		&curHash,
		&curSrcRev,
	)
	if err != nil {
		return Skill{}, err
	}
	s.UpstreamURL = nullStrPtr(upURL)
	s.UpstreamRef = nullStrPtr(upRef)
	s.UpstreamSubpath = nullStrPtr(upSub)
	s.UpstreamLatestRev = nullStrPtr(upRev)
	s.UpstreamLatestMessage = nullStrPtr(upMsg)
	s.UpstreamCheckedAt = nullStrPtr(upCheck)
	s.CurrentVersionID = nullStrPtr(curVerID)
	s.CurrentVersionNum = nullIntPtr(curVerNum)
	s.CurrentContentHash = nullStrPtr(curHash)
	s.CurrentSourceRev = nullStrPtr(curSrcRev)
	s.HasUpstreamUpdate = s.UpstreamLatestRev != nil &&
		s.CurrentSourceRev != nil &&
		*s.UpstreamLatestRev != *s.CurrentSourceRev
	return s, nil
}

func scanVersion(scanner rowScanner, includeFiles bool) (SkillVersion, error) {
	var (
		v         SkillVersion
		srcRev    sql.NullString
		filesJSON string
	)
	err := scanner.Scan(
		&v.ID,
		&v.SkillID,
		&v.VersionNum,
		&v.ContentHash,
		&srcRev,
		&v.Note,
		&filesJSON,
		&v.CreatedAt,
	)
	if err != nil {
		return SkillVersion{}, err
	}
	v.SourceRev = nullStrPtr(srcRev)
	if includeFiles {
		v.Files = map[string]skill.FileEntry{}
		if filesJSON != "" {
			_ = json.Unmarshal([]byte(filesJSON), &v.Files)
		}
	}
	return v, nil
}

func scanDevice(scanner rowScanner) (Device, error) {
	var (
		d          Device
		hostname   sql.NullString
		agentsJSON string
	)
	err := scanner.Scan(
		&d.ID,
		&d.Name,
		&d.OS,
		&d.Arch,
		&hostname,
		&agentsJSON,
		&d.LastSeenAt,
		&d.CreatedAt,
	)
	if err != nil {
		return Device{}, err
	}
	d.Hostname = nullStrPtr(hostname)
	d.DetectedAgents = make([]DetectedAgent, 0)
	if agentsJSON != "" {
		_ = json.Unmarshal([]byte(agentsJSON), &d.DetectedAgents)
	}
	return d, nil
}

func scanInstallation(scanner rowScanner, includeFiles bool) (Installation, error) {
	var (
		inst                            Installation
		vaultID, desiredVerID           sql.NullString
		baseHash, actualHash            sql.NullString
		ignoredInt                      int
		metaJSON, filesJSON             sql.NullString
		devName, devOS, vaultName, dHsh sql.NullString
		desiredNum                      sql.NullInt64
	)
	err := scanner.Scan(
		&inst.ID,
		&inst.DeviceID,
		&inst.AgentID,
		&inst.SkillName,
		&vaultID,
		&desiredVerID,
		&baseHash,
		&actualHash,
		&inst.Status,
		&ignoredInt,
		&metaJSON,
		&filesJSON,
		&inst.LastScannedAt,
		&devName,
		&devOS,
		&vaultName,
		&desiredNum,
		&dHsh,
	)
	if err != nil {
		return Installation{}, err
	}
	inst.VaultSkillID = nullStrPtr(vaultID)
	inst.DesiredVersionID = nullStrPtr(desiredVerID)
	inst.BaseHash = nullStrPtr(baseHash)
	inst.ActualHash = nullStrPtr(actualHash)
	inst.Ignored = ignoredInt != 0
	inst.DeviceName = nullStrPtr(devName)
	inst.DeviceOS = nullStrPtr(devOS)
	inst.VaultSkillName = nullStrPtr(vaultName)
	inst.DesiredVersionNum = nullIntPtr(desiredNum)
	inst.DesiredHash = nullStrPtr(dHsh)
	inst.HasActualFiles = filesJSON.Valid && filesJSON.String != ""

	if metaJSON.Valid && metaJSON.String != "" {
		var m skill.SkillMeta
		if json.Unmarshal([]byte(metaJSON.String), &m) == nil {
			inst.ActualMeta = &m
		}
	}
	if includeFiles && filesJSON.Valid && filesJSON.String != "" {
		files := map[string]skill.FileEntry{}
		if json.Unmarshal([]byte(filesJSON.String), &files) == nil {
			inst.ActualFiles = files
		}
	}
	return inst, nil
}

func scanAction(scanner rowScanner) (Action, error) {
	var (
		a           Action
		payloadJSON string
		errMsg      sql.NullString
	)
	err := scanner.Scan(
		&a.ID,
		&a.DeviceID,
		&a.AgentID,
		&a.SkillName,
		&a.Type,
		&payloadJSON,
		&a.Status,
		&errMsg,
		&a.CreatedAt,
		&a.UpdatedAt,
	)
	if err != nil {
		return Action{}, err
	}
	a.ErrorMessage = nullStrPtr(errMsg)
	a.Payload = map[string]any{}
	if payloadJSON != "" {
		_ = json.Unmarshal([]byte(payloadJSON), &a.Payload)
	}
	return a, nil
}

// ListSkills reads all Vault skills from ReadDB.
func (s *Store) ListSkills(ctx context.Context) ([]Skill, error) {
	rows, err := s.db.ReadDB.QueryContext(ctx, `
		SELECT s.id, s.name, s.description, s.source_type,
		       s.upstream_url, s.upstream_ref, s.upstream_subpath,
		       s.upstream_latest_rev, s.upstream_latest_message, s.upstream_checked_at,
		       s.current_version_id, s.created_at, s.updated_at,
		       sv.version_num, sv.content_hash, sv.source_rev
		FROM skills s
		LEFT JOIN skill_versions sv ON sv.id = s.current_version_id
		ORDER BY s.updated_at DESC, s.name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	skills := make([]Skill, 0)
	for rows.Next() {
		sk, err := scanSkill(rows)
		if err != nil {
			return nil, err
		}
		skills = append(skills, sk)
	}
	return skills, rows.Err()
}

// GetSkill reads a single Vault skill from ReadDB.
func (s *Store) GetSkill(ctx context.Context, skillID string, includeVersions, includeCurrentFiles bool) (*Skill, error) {
	return s.getSkillWithRunner(ctx, s.db.ReadDB, skillID, includeVersions, includeCurrentFiles)
}

func (s *Store) getSkillWithRunner(ctx context.Context, runner queryRunner, skillID string, includeVersions, includeCurrentFiles bool) (*Skill, error) {
	row := runner.QueryRowContext(ctx, `
		SELECT s.id, s.name, s.description, s.source_type,
		       s.upstream_url, s.upstream_ref, s.upstream_subpath,
		       s.upstream_latest_rev, s.upstream_latest_message, s.upstream_checked_at,
		       s.current_version_id, s.created_at, s.updated_at,
		       sv.version_num, sv.content_hash, sv.source_rev
		FROM skills s
		LEFT JOIN skill_versions sv ON sv.id = s.current_version_id
		WHERE s.id = ?`, skillID)
	sk, err := scanSkill(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	if sk.CurrentVersionID != nil {
		ver, err := s.getVersionWithRunner(ctx, runner, *sk.CurrentVersionID, includeCurrentFiles)
		if err != nil {
			return nil, err
		}
		sk.CurrentVersion = ver
	}

	if includeVersions {
		vers, err := s.listSkillVersionsWithRunner(ctx, runner, skillID, false)
		if err != nil {
			return nil, err
		}
		sk.Versions = vers
	}
	return &sk, nil
}

func (s *Store) listSkillVersionsWithRunner(ctx context.Context, runner queryRunner, skillID string, includeFiles bool) ([]SkillVersion, error) {
	rows, err := runner.QueryContext(ctx, `
		SELECT id, skill_id, version_num, content_hash, source_rev, note, files_json, created_at
		FROM skill_versions
		WHERE skill_id = ?
		ORDER BY version_num DESC`, skillID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	versions := make([]SkillVersion, 0)
	for rows.Next() {
		v, err := scanVersion(rows, includeFiles)
		if err != nil {
			return nil, err
		}
		versions = append(versions, v)
	}
	return versions, rows.Err()
}

// GetVersion reads a specific skill version from ReadDB.
func (s *Store) GetVersion(ctx context.Context, versionID string, includeFiles bool) (*SkillVersion, error) {
	return s.getVersionWithRunner(ctx, s.db.ReadDB, versionID, includeFiles)
}

func (s *Store) getVersionWithRunner(ctx context.Context, runner queryRunner, versionID string, includeFiles bool) (*SkillVersion, error) {
	row := runner.QueryRowContext(ctx, `
		SELECT id, skill_id, version_num, content_hash, source_rev, note, files_json, created_at
		FROM skill_versions WHERE id = ?`, versionID)
	v, err := scanVersion(row, includeFiles)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// FindVersionByHash resolves a version snapshot matching contentHash for a skill via ReadDB.
func (s *Store) FindVersionByHash(ctx context.Context, skillID, contentHash string) (*SkillVersion, error) {
	if skillID == "" || contentHash == "" {
		return nil, nil
	}
	row := s.db.ReadDB.QueryRowContext(ctx, `
		SELECT id, skill_id, version_num, content_hash, source_rev, note, files_json, created_at
		FROM skill_versions
		WHERE skill_id = ? AND content_hash = ?
		ORDER BY version_num DESC
		LIMIT 1`, skillID, contentHash)
	v, err := scanVersion(row, true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// CreateSkillWithVersion atomically inserts a new skill and v1 snapshot on WriteDB.
func (s *Store) CreateSkillWithVersion(ctx context.Context, in CreateSkillInput) (*Skill, error) {
	now := skill.ISONow()
	versionID := skill.GenerateID("ver")
	filesBytes, err := json.Marshal(in.Files)
	if err != nil {
		return nil, err
	}
	sourceType := in.SourceType
	if sourceType == "" {
		sourceType = "custom"
	}
	note := in.Note
	if note == "" {
		note = "Initial version"
	}

	var out *Skill
	err = s.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO skills (
				id, name, description, source_type,
				upstream_url, upstream_ref, upstream_subpath,
				upstream_latest_rev, upstream_latest_message, upstream_checked_at,
				current_version_id, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			in.ID,
			in.Name,
			in.Description,
			sourceType,
			strPtrToNull(in.UpstreamURL),
			strPtrToNull(in.UpstreamRef),
			strPtrToNull(in.UpstreamSubpath),
			strPtrToNull(in.UpstreamLatestRev),
			strPtrToNull(in.UpstreamLatestMessage),
			strPtrToNull(in.UpstreamCheckedAt),
			versionID,
			now,
			now,
		); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "PRIMARY KEY") {
				return ErrConflict
			}
			return err
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO skill_versions (
				id, skill_id, version_num, content_hash, source_rev, note, files_json, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			versionID, in.ID, 1, in.ContentHash, strPtrToNull(in.SourceRev), note, string(filesBytes), now,
		); err != nil {
			return err
		}

		out, err = s.getSkillWithRunner(ctx, tx, in.ID, true, true)
		return err
	})
	return out, err
}

// AddSkillVersion appends a new version and recomputes linked installations in a single WriteDB transaction.
func (s *Store) AddSkillVersion(ctx context.Context, skillID string, files map[string]skill.FileEntry, contentHash string, sourceRev *string, note string) (*SkillVersion, error) {
	filesBytes, err := json.Marshal(files)
	if err != nil {
		return nil, err
	}

	var created *SkillVersion
	err = s.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM skills WHERE id = ?`, skillID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return ErrNotFound
		}

		var maxNum int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version_num), 0) FROM skill_versions WHERE skill_id = ?`, skillID).Scan(&maxNum); err != nil {
			return err
		}
		nextNum := maxNum + 1
		versionID := skill.GenerateID("ver")
		now := skill.ISONow()

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO skill_versions (
				id, skill_id, version_num, content_hash, source_rev, note, files_json, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			versionID, skillID, nextNum, contentHash, strPtrToNull(sourceRev), note, string(filesBytes), now,
		); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE skills SET current_version_id = ?, updated_at = ? WHERE id = ?`,
			versionID, now, skillID,
		); err != nil {
			return err
		}

		if err := recomputeInstallationsTx(ctx, tx, skillID, versionID, contentHash); err != nil {
			return err
		}

		created, err = s.getVersionWithRunner(ctx, tx, versionID, true)
		return err
	})
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return created, err
}

func recomputeInstallationsTx(ctx context.Context, tx *sql.Tx, skillID, desiredVersionID, desiredHash string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, base_hash, actual_hash FROM installations WHERE vault_skill_id = ?`, skillID)
	if err != nil {
		return err
	}
	type instLite struct {
		id         string
		baseHash   sql.NullString
		actualHash sql.NullString
	}
	var items []instLite
	for rows.Next() {
		var it instLite
		if err := rows.Scan(&it.id, &it.baseHash, &it.actualHash); err != nil {
			_ = rows.Close()
			return err
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	for _, it := range items {
		nextStatus := skill.ComputeInstallationStatus(it.baseHash.String, it.actualHash.String, desiredHash, true)
		if _, err := tx.ExecContext(ctx,
			`UPDATE installations SET desired_version_id = ?, status = ? WHERE id = ?`,
			desiredVersionID, nextStatus, it.id,
		); err != nil {
			return err
		}
	}
	return nil
}

// SkillMetaPatch contains optional skill metadata updates.
type SkillMetaPatch struct {
	Name            *string
	Description     *string
	UpstreamSet     bool
	UpstreamURL     *string
	UpstreamRef     *string
	UpstreamSubpath *string
}

// UpdateSkillMeta updates editable skill fields on WriteDB.
func (s *Store) UpdateSkillMeta(ctx context.Context, skillID string, patch SkillMetaPatch) (*Skill, error) {
	var updated *Skill
	err := s.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		existing, err := s.getSkillWithRunner(ctx, tx, skillID, false, false)
		if err != nil {
			return err
		}
		if existing == nil {
			return ErrNotFound
		}
		name := existing.Name
		if patch.Name != nil && strings.TrimSpace(*patch.Name) != "" {
			name = strings.TrimSpace(*patch.Name)
		}
		desc := existing.Description
		if patch.Description != nil {
			desc = *patch.Description
		}
		upURL, upRef, upSub := existing.UpstreamURL, existing.UpstreamRef, existing.UpstreamSubpath
		if patch.UpstreamSet {
			upURL = patch.UpstreamURL
			upRef = patch.UpstreamRef
			upSub = patch.UpstreamSubpath
		}
		now := skill.ISONow()
		if _, err := tx.ExecContext(ctx, `
			UPDATE skills
			SET name = ?, description = ?, upstream_url = ?, upstream_ref = ?, upstream_subpath = ?, updated_at = ?
			WHERE id = ?`,
			name, desc, strPtrToNull(upURL), strPtrToNull(upRef), strPtrToNull(upSub), now, skillID,
		); err != nil {
			return err
		}
		updated, err = s.getSkillWithRunner(ctx, tx, skillID, true, true)
		return err
	})
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return updated, err
}

// UpdateSkillUpstream persists latest GitHub upstream check metadata on WriteDB.
func (s *Store) UpdateSkillUpstream(ctx context.Context, skillID, latestRev, latestMsg, checkedAt string) (*Skill, error) {
	if checkedAt == "" {
		checkedAt = skill.ISONow()
	}
	var updated *Skill
	err := s.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE skills
			SET upstream_latest_rev = ?, upstream_latest_message = ?, upstream_checked_at = ?
			WHERE id = ?`,
			latestRev, latestMsg, checkedAt, skillID,
		); err != nil {
			return err
		}
		var err error
		updated, err = s.getSkillWithRunner(ctx, tx, skillID, true, true)
		return err
	})
	return updated, err
}

// DeleteSkill unlinks or removes affected installations and deletes the Vault skill on WriteDB.
func (s *Store) DeleteSkill(ctx context.Context, skillID string) (bool, error) {
	deleted := false
	err := s.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM skills WHERE id = ?`, skillID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return nil
		}

		rows, err := tx.QueryContext(ctx, `SELECT id, actual_hash FROM installations WHERE vault_skill_id = ?`, skillID)
		if err != nil {
			return err
		}
		type affected struct {
			id         string
			actualHash sql.NullString
		}
		var list []affected
		for rows.Next() {
			var a affected
			if err := rows.Scan(&a.id, &a.actualHash); err != nil {
				_ = rows.Close()
				return err
			}
			list = append(list, a)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()

		for _, a := range list {
			if a.actualHash.Valid && a.actualHash.String != "" {
				if _, err := tx.ExecContext(ctx, `
					UPDATE installations
					SET vault_skill_id = NULL, desired_version_id = NULL, base_hash = NULL, status = 'unmanaged'
					WHERE id = ?`, a.id,
				); err != nil {
					return err
				}
			} else {
				if _, err := tx.ExecContext(ctx, `DELETE FROM installations WHERE id = ?`, a.id); err != nil {
					return err
				}
			}
		}

		if _, err := tx.ExecContext(ctx, `DELETE FROM skills WHERE id = ?`, skillID); err != nil {
			return err
		}
		deleted = true
		return nil
	})
	return deleted, err
}

// UpsertDevice inserts or updates a registered device on WriteDB.
func (s *Store) UpsertDevice(ctx context.Context, id, name, osName, arch string, hostname *string, detectedAgents []DetectedAgent) (*Device, error) {
	if detectedAgents == nil {
		detectedAgents = make([]DetectedAgent, 0)
	}
	agentsBytes, err := json.Marshal(detectedAgents)
	if err != nil {
		return nil, err
	}
	now := skill.ISONow()

	var dev *Device
	err = s.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO devices (id, name, os, arch, hostname, detected_agents_json, last_seen_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				name = excluded.name,
				os = excluded.os,
				arch = excluded.arch,
				hostname = excluded.hostname,
				detected_agents_json = excluded.detected_agents_json,
				last_seen_at = excluded.last_seen_at`,
			id, name, osName, arch, strPtrToNull(hostname), string(agentsBytes), now, now,
		); err != nil {
			return err
		}
		dev, err = s.getDeviceWithRunner(ctx, tx, id)
		return err
	})
	return dev, err
}

// ListDevices queries all devices via ReadDB.
func (s *Store) ListDevices(ctx context.Context) ([]Device, error) {
	rows, err := s.db.ReadDB.QueryContext(ctx, `
		SELECT id, name, os, arch, hostname, detected_agents_json, last_seen_at, created_at
		FROM devices
		ORDER BY last_seen_at DESC, name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	devices := make([]Device, 0)
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		devices = append(devices, d)
	}
	return devices, rows.Err()
}

// GetDevice queries a device by ID via ReadDB.
func (s *Store) GetDevice(ctx context.Context, deviceID string) (*Device, error) {
	return s.getDeviceWithRunner(ctx, s.db.ReadDB, deviceID)
}

func (s *Store) getDeviceWithRunner(ctx context.Context, runner queryRunner, deviceID string) (*Device, error) {
	row := runner.QueryRowContext(ctx, `
		SELECT id, name, os, arch, hostname, detected_agents_json, last_seen_at, created_at
		FROM devices WHERE id = ?`, deviceID)
	d, err := scanDevice(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// DeleteDevice removes a device and cascades installations/actions via WriteDB.
func (s *Store) DeleteDevice(ctx context.Context, deviceID string) (bool, error) {
	deleted := false
	err := s.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM devices WHERE id = ?`, deviceID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		deleted = n > 0
		return nil
	})
	return deleted, err
}

// ReplaceDeviceInstallations reconciles a device's reported skill scan against Vault state in one WriteDB tx.
func (s *Store) ReplaceDeviceInstallations(ctx context.Context, deviceID string, scannedSkills []ScannedSkillInput) error {
	now := skill.ISONow()

	return s.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		type prevInst struct {
			id              string
			agentID         string
			skillName       string
			vaultSkillID    sql.NullString
			ignored         int
			actualMetaJSON  sql.NullString
			actualFilesJSON sql.NullString
		}
		existingByKey := make(map[string]prevInst)
		var existingList []prevInst

		rows, err := tx.QueryContext(ctx, `
			SELECT id, agent_id, skill_name, vault_skill_id, ignored, actual_meta_json, actual_files_json
			FROM installations WHERE device_id = ?`, deviceID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var p prevInst
			if err := rows.Scan(&p.id, &p.agentID, &p.skillName, &p.vaultSkillID, &p.ignored, &p.actualMetaJSON, &p.actualFilesJSON); err != nil {
				_ = rows.Close()
				return err
			}
			existingByKey[p.agentID+":"+p.skillName] = p
			existingList = append(existingList, p)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()

		type skillRef struct {
			currentVersionID   sql.NullString
			currentContentHash sql.NullString
		}
		skillsByID := make(map[string]skillRef)
		sRows, err := tx.QueryContext(ctx, `
			SELECT s.id, s.current_version_id, sv.content_hash
			FROM skills s
			LEFT JOIN skill_versions sv ON sv.id = s.current_version_id`)
		if err != nil {
			return err
		}
		for sRows.Next() {
			var id string
			var ref skillRef
			if err := sRows.Scan(&id, &ref.currentVersionID, &ref.currentContentHash); err != nil {
				_ = sRows.Close()
				return err
			}
			skillsByID[id] = ref
		}
		if err := sRows.Err(); err != nil {
			_ = sRows.Close()
			return err
		}
		_ = sRows.Close()

		type adoptPayload struct {
			VaultSkillID string `json:"vaultSkillId"`
			ExpectedHash string `json:"expectedHash"`
		}
		activeAdoptByKey := make(map[string]adoptPayload)
		aRows, err := tx.QueryContext(ctx, `
			SELECT agent_id, skill_name, payload_json
			FROM actions
			WHERE device_id = ? AND type = ? AND status IN (?, ?)`,
			deviceID, skill.ActionAdoptConfirm, skill.ActionStatusPending, skill.ActionStatusClaimed,
		)
		if err != nil {
			return err
		}
		for aRows.Next() {
			var agID, skName, pJSON string
			if err := aRows.Scan(&agID, &skName, &pJSON); err != nil {
				_ = aRows.Close()
				return err
			}
			var ap adoptPayload
			if json.Unmarshal([]byte(pJSON), &ap) == nil {
				activeAdoptByKey[agID+":"+skName] = ap
			}
		}
		if err := aRows.Err(); err != nil {
			_ = aRows.Close()
			return err
		}
		_ = aRows.Close()

		seenKeys := make(map[string]struct{}, len(scannedSkills))
		upsertStmt, err := tx.PrepareContext(ctx, `
			INSERT INTO installations (
				id, device_id, agent_id, skill_name,
				vault_skill_id, desired_version_id, base_hash, actual_hash,
				status, ignored, actual_meta_json, actual_files_json, last_scanned_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(device_id, agent_id, skill_name) DO UPDATE SET
				vault_skill_id = excluded.vault_skill_id,
				desired_version_id = excluded.desired_version_id,
				base_hash = excluded.base_hash,
				actual_hash = excluded.actual_hash,
				status = excluded.status,
				actual_meta_json = excluded.actual_meta_json,
				actual_files_json = excluded.actual_files_json,
				last_scanned_at = excluded.last_scanned_at`)
		if err != nil {
			return err
		}
		defer upsertStmt.Close()

		for _, item := range scannedSkills {
			key := item.AgentID + ":" + item.SkillName
			seenKeys[key] = struct{}{}
			prev, hasPrev := existingByKey[key]

			var vaultSkillID string
			if item.VaultSkillID != nil {
				if _, ok := skillsByID[*item.VaultSkillID]; ok {
					vaultSkillID = *item.VaultSkillID
				}
			}
			if vaultSkillID == "" && hasPrev && prev.vaultSkillID.Valid {
				if _, ok := skillsByID[prev.vaultSkillID.String]; ok {
					vaultSkillID = prev.vaultSkillID.String
				}
			}

			var desiredVersionID, desiredHash string
			if vaultSkillID != "" {
				skRef := skillsByID[vaultSkillID]
				desiredVersionID = skRef.currentVersionID.String
				desiredHash = skRef.currentContentHash.String
			}

			var baseHash, actualHash string
			if item.BaseHash != nil {
				baseHash = *item.BaseHash
			}
			if item.ActualHash != nil {
				actualHash = *item.ActualHash
			}

			if pendingAdopt, ok := activeAdoptByKey[key]; ok {
				if actualHash != "" && pendingAdopt.ExpectedHash == actualHash && pendingAdopt.VaultSkillID != "" {
					if skRef, ok2 := skillsByID[pendingAdopt.VaultSkillID]; ok2 {
						vaultSkillID = pendingAdopt.VaultSkillID
						baseHash = actualHash
						desiredVersionID = skRef.currentVersionID.String
						desiredHash = skRef.currentContentHash.String
					}
				}
			}

			isManaged := vaultSkillID != "" || baseHash != ""
			status := skill.ComputeInstallationStatus(baseHash, actualHash, desiredHash, isManaged)

			var actualFilesNull sql.NullString
			if status == skill.InstallStatusUnmanaged || status == skill.InstallStatusModified {
				if item.Files != nil {
					if b, err := json.Marshal(item.Files); err == nil {
						actualFilesNull = sql.NullString{String: string(b), Valid: true}
					}
				} else if hasPrev && prev.actualFilesJSON.Valid {
					actualFilesNull = prev.actualFilesJSON
				}
			}

			var actualMetaNull sql.NullString
			if item.Meta != nil {
				if b, err := json.Marshal(item.Meta); err == nil {
					actualMetaNull = sql.NullString{String: string(b), Valid: true}
				}
			} else if hasPrev && prev.actualMetaJSON.Valid {
				actualMetaNull = prev.actualMetaJSON
			}

			ignored := 0
			if hasPrev {
				ignored = prev.ignored
			}
			rowID := deviceID + ":" + item.AgentID + ":" + item.SkillName

			if _, err := upsertStmt.ExecContext(ctx,
				rowID,
				deviceID,
				item.AgentID,
				item.SkillName,
				strValToNull(vaultSkillID),
				strValToNull(desiredVersionID),
				strValToNull(baseHash),
				strValToNull(actualHash),
				status,
				ignored,
				actualMetaNull,
				actualFilesNull,
				now,
			); err != nil {
				return err
			}
		}

		for _, prev := range existingList {
			key := prev.agentID + ":" + prev.skillName
			if _, ok := seenKeys[key]; !ok {
				if _, err := tx.ExecContext(ctx, `DELETE FROM installations WHERE id = ?`, prev.id); err != nil {
					return err
				}
			}
		}

		return nil
	})
}

// ListInstallations queries installations via ReadDB with optional deviceId or skillId filter.
func (s *Store) ListInstallations(ctx context.Context, deviceID, skillID string) ([]Installation, error) {
	var conds []string
	var args []any
	if deviceID != "" {
		conds = append(conds, "i.device_id = ?")
		args = append(args, deviceID)
	}
	if skillID != "" {
		conds = append(conds, "i.vault_skill_id = ?")
		args = append(args, skillID)
	}
	whereClause := ""
	if len(conds) > 0 {
		whereClause = "WHERE " + strings.Join(conds, " AND ")
	}

	q := fmt.Sprintf(`
		SELECT i.id, i.device_id, i.agent_id, i.skill_name,
		       i.vault_skill_id, i.desired_version_id, i.base_hash, i.actual_hash,
		       i.status, i.ignored, i.actual_meta_json, i.actual_files_json, i.last_scanned_at,
		       d.name, d.os, s.name, sv.version_num, sv.content_hash
		FROM installations i
		LEFT JOIN devices d ON d.id = i.device_id
		LEFT JOIN skills s ON s.id = i.vault_skill_id
		LEFT JOIN skill_versions sv ON sv.id = i.desired_version_id
		%s
		ORDER BY i.device_id ASC, i.agent_id ASC, i.skill_name ASC`, whereClause)

	rows, err := s.db.ReadDB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Installation, 0)
	for rows.Next() {
		inst, err := scanInstallation(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	return out, rows.Err()
}

// GetInstallation fetches a single installation via ReadDB.
func (s *Store) GetInstallation(ctx context.Context, deviceID, agentID, skillName string, includeFiles bool) (*Installation, error) {
	return s.getInstallationWithRunner(ctx, s.db.ReadDB, deviceID, agentID, skillName, includeFiles)
}

func (s *Store) getInstallationWithRunner(ctx context.Context, runner queryRunner, deviceID, agentID, skillName string, includeFiles bool) (*Installation, error) {
	row := runner.QueryRowContext(ctx, `
		SELECT i.id, i.device_id, i.agent_id, i.skill_name,
		       i.vault_skill_id, i.desired_version_id, i.base_hash, i.actual_hash,
		       i.status, i.ignored, i.actual_meta_json, i.actual_files_json, i.last_scanned_at,
		       d.name, d.os, s.name, sv.version_num, sv.content_hash
		FROM installations i
		LEFT JOIN devices d ON d.id = i.device_id
		LEFT JOIN skills s ON s.id = i.vault_skill_id
		LEFT JOIN skill_versions sv ON sv.id = i.desired_version_id
		WHERE i.device_id = ? AND i.agent_id = ? AND i.skill_name = ?`,
		deviceID, agentID, skillName,
	)
	inst, err := scanInstallation(row, includeFiles)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &inst, nil
}

// InstallationPatch updates selective installation fields inside a WriteDB transaction.
type InstallationPatch struct {
	SetVaultSkillID     bool
	VaultSkillID        *string
	SetDesiredVersionID bool
	DesiredVersionID    *string
	SetBaseHash         bool
	BaseHash            *string
	SetStatus           bool
	Status              string
	SetIgnored          bool
	Ignored             bool
	ClearActualFiles    bool
	SetActualFiles      bool
	ActualFiles         map[string]skill.FileEntry
}

// UpdateInstallation mutates installation metadata on WriteDB.
func (s *Store) UpdateInstallation(ctx context.Context, deviceID, agentID, skillName string, patch InstallationPatch) (*Installation, error) {
	var updated *Installation
	err := s.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		existing, err := s.getInstallationWithRunner(ctx, tx, deviceID, agentID, skillName, true)
		if err != nil {
			return err
		}
		if existing == nil {
			return ErrNotFound
		}

		vaultSkillID := existing.VaultSkillID
		if patch.SetVaultSkillID {
			vaultSkillID = patch.VaultSkillID
		}
		desiredVersionID := existing.DesiredVersionID
		if patch.SetDesiredVersionID {
			desiredVersionID = patch.DesiredVersionID
		}
		baseHash := existing.BaseHash
		if patch.SetBaseHash {
			baseHash = patch.BaseHash
		}
		status := existing.Status
		if patch.SetStatus {
			status = patch.Status
		}
		ignoredInt := 0
		if (patch.SetIgnored && patch.Ignored) || (!patch.SetIgnored && existing.Ignored) {
			ignoredInt = 1
		}

		var actualFilesNull sql.NullString
		if !patch.ClearActualFiles {
			if patch.SetActualFiles {
				if patch.ActualFiles != nil {
					if b, err := json.Marshal(patch.ActualFiles); err == nil {
						actualFilesNull = sql.NullString{String: string(b), Valid: true}
					}
				}
			} else if existing.ActualFiles != nil {
				if b, err := json.Marshal(existing.ActualFiles); err == nil {
					actualFilesNull = sql.NullString{String: string(b), Valid: true}
				}
			}
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE installations
			SET vault_skill_id = ?, desired_version_id = ?, base_hash = ?, status = ?, ignored = ?, actual_files_json = ?
			WHERE device_id = ? AND agent_id = ? AND skill_name = ?`,
			strPtrToNull(vaultSkillID),
			strPtrToNull(desiredVersionID),
			strPtrToNull(baseHash),
			status,
			ignoredInt,
			actualFilesNull,
			deviceID,
			agentID,
			skillName,
		); err != nil {
			return err
		}
		updated, err = s.getInstallationWithRunner(ctx, tx, deviceID, agentID, skillName, true)
		return err
	})
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return updated, err
}

// CreateAction inserts a pending action for a device on WriteDB.
func (s *Store) CreateAction(ctx context.Context, deviceID, agentID, skillName, actionType string, payload map[string]any) (*Action, error) {
	if payload == nil {
		payload = map[string]any{}
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	id := skill.GenerateID("act")
	now := skill.ISONow()

	var created *Action
	err = s.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO actions (
				id, device_id, agent_id, skill_name, type, payload_json, status, error_message, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)`,
			id, deviceID, agentID, skillName, actionType, string(payloadBytes), skill.ActionStatusPending, now, now,
		); err != nil {
			return err
		}
		created, err = s.getActionWithRunner(ctx, tx, id)
		return err
	})
	return created, err
}

func (s *Store) getActionWithRunner(ctx context.Context, runner queryRunner, actionID string) (*Action, error) {
	row := runner.QueryRowContext(ctx, `
		SELECT id, device_id, agent_id, skill_name, type, payload_json, status, error_message, created_at, updated_at
		FROM actions WHERE id = ?`, actionID)
	a, err := scanAction(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ListActions reads action queue items via ReadDB.
func (s *Store) ListActions(ctx context.Context, deviceID, status string, limit int) ([]Action, error) {
	if limit <= 0 {
		limit = 100
	}
	var conds []string
	var args []any
	if deviceID != "" {
		conds = append(conds, "device_id = ?")
		args = append(args, deviceID)
	}
	if status != "" {
		conds = append(conds, "status = ?")
		args = append(args, status)
	}
	whereClause := ""
	if len(conds) > 0 {
		whereClause = "WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, limit)

	rows, err := s.db.ReadDB.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, device_id, agent_id, skill_name, type, payload_json, status, error_message, created_at, updated_at
		FROM actions %s
		ORDER BY created_at DESC LIMIT ?`, whereClause), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	actions := make([]Action, 0)
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, err
		}
		actions = append(actions, a)
	}
	return actions, rows.Err()
}

// CountPendingActions counts pending actions for a device via ReadDB.
func (s *Store) CountPendingActions(ctx context.Context, deviceID string) (int, error) {
	var cnt int
	err := s.db.ReadDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM actions WHERE device_id = ? AND status = ?`,
		deviceID, skill.ActionStatusPending,
	).Scan(&cnt)
	return cnt, err
}

// ClaimPendingActions atomically transitions pending actions to claimed on WriteDB and hydrates version files.
func (s *Store) ClaimPendingActions(ctx context.Context, deviceID string) ([]Action, error) {
	now := skill.ISONow()
	actions := make([]Action, 0)

	err := s.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT id, device_id, agent_id, skill_name, type, payload_json, status, error_message, created_at, updated_at
			FROM actions
			WHERE device_id = ? AND status = ?
			ORDER BY created_at ASC`, deviceID, skill.ActionStatusPending)
		if err != nil {
			return err
		}
		var claimed []Action
		for rows.Next() {
			a, err := scanAction(rows)
			if err != nil {
				_ = rows.Close()
				return err
			}
			claimed = append(claimed, a)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()

		if len(claimed) == 0 {
			return nil
		}

		for i := range claimed {
			if _, err := tx.ExecContext(ctx,
				`UPDATE actions SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
				skill.ActionStatusClaimed, now, claimed[i].ID, skill.ActionStatusPending,
			); err != nil {
				return err
			}
			claimed[i].Status = skill.ActionStatusClaimed
			claimed[i].UpdatedAt = now

			if claimed[i].Type == skill.ActionInstall ||
				claimed[i].Type == skill.ActionUpdate ||
				claimed[i].Type == skill.ActionReset {
				versionID, _ := claimed[i].Payload["versionId"].(string)
				if versionID == "" {
					if vaultSkillID, _ := claimed[i].Payload["vaultSkillId"].(string); vaultSkillID != "" {
						sk, err := s.getSkillWithRunner(ctx, tx, vaultSkillID, false, false)
						if err != nil {
							return err
						}
						if sk != nil && sk.CurrentVersionID != nil {
							versionID = *sk.CurrentVersionID
						}
					}
				}
				if versionID != "" {
					ver, err := s.getVersionWithRunner(ctx, tx, versionID, true)
					if err != nil {
						return err
					}
					if ver != nil {
						claimed[i].Payload["versionId"] = ver.ID
						claimed[i].Payload["versionNum"] = ver.VersionNum
						claimed[i].Payload["contentHash"] = ver.ContentHash
						claimed[i].Payload["files"] = ver.Files
					}
				}
			}
		}

		actions = claimed
		return nil
	})
	return actions, err
}

// CompleteAction marks an action completed or failed on WriteDB.
func (s *Store) CompleteAction(ctx context.Context, actionID, status string, errorMessage *string) (*Action, error) {
	nextStatus := skill.ActionStatusFailed
	if status == skill.ActionStatusCompleted {
		nextStatus = skill.ActionStatusCompleted
	}
	now := skill.ISONow()

	var out *Action
	err := s.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		existing, err := s.getActionWithRunner(ctx, tx, actionID)
		if err != nil {
			return err
		}
		if existing == nil {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE actions SET status = ?, error_message = ?, updated_at = ? WHERE id = ?`,
			nextStatus, strPtrToNull(errorMessage), now, actionID,
		); err != nil {
			return err
		}
		out, err = s.getActionWithRunner(ctx, tx, actionID)
		return err
	})
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return out, err
}

// CancelAction cancels a pending or claimed action on WriteDB.
func (s *Store) CancelAction(ctx context.Context, actionID string) (*Action, error) {
	now := skill.ISONow()
	var out *Action
	err := s.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		existing, err := s.getActionWithRunner(ctx, tx, actionID)
		if err != nil {
			return err
		}
		if existing == nil {
			return ErrNotFound
		}
		if existing.Status != skill.ActionStatusPending && existing.Status != skill.ActionStatusClaimed {
			out = existing
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE actions SET status = ?, updated_at = ? WHERE id = ?`,
			skill.ActionStatusCancelled, now, actionID,
		); err != nil {
			return err
		}
		out, err = s.getActionWithRunner(ctx, tx, actionID)
		return err
	})
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return out, err
}

// --- High-Concurrency Engine Persistence Helpers ---

// LoadSystemConfig reads the active dynamic config from ReadDB or returns defaults.
func (s *Store) LoadSystemConfig(ctx context.Context) (hotstate.SystemDynamicConfig, error) {
	var raw string
	var ver int64
	err := s.db.ReadDB.QueryRowContext(ctx,
		`SELECT value_json, version FROM system_configs WHERE key = 'runtime'`,
	).Scan(&raw, &ver)
	if errors.Is(err, sql.ErrNoRows) {
		return hotstate.DefaultSystemConfig(), nil
	}
	if err != nil {
		return hotstate.DefaultSystemConfig(), err
	}
	cfg := hotstate.DefaultSystemConfig()
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return hotstate.DefaultSystemConfig(), err
	}
	cfg.Version = ver
	if cfg.RouteOverrides == nil {
		cfg.RouteOverrides = map[string]bool{}
	}
	return cfg, nil
}

// SaveSystemConfig persists dynamic runtime settings on WriteDB.
func (s *Store) SaveSystemConfig(ctx context.Context, cfg hotstate.SystemDynamicConfig) (hotstate.SystemDynamicConfig, error) {
	if cfg.RouteOverrides == nil {
		cfg.RouteOverrides = map[string]bool{}
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return cfg, err
	}
	now := skill.ISONow()
	err = s.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO system_configs (key, value_json, version, updated_at)
			VALUES ('runtime', ?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET
				value_json = excluded.value_json,
				version = excluded.version,
				updated_at = excluded.updated_at`,
			string(raw), cfg.Version, now,
		)
		return err
	})
	return cfg, err
}

// EnsureRootAccount persists or updates the primary root token row so async ledger updates always have a target row.
func (s *Store) EnsureRootAccount(ctx context.Context, tokenHashHex, tokenPrefix string) error {
	now := skill.ISONow()
	return s.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO api_accounts (
				id, name, token_hash, token_prefix, role, enabled, unlimited_quota,
				balance_micros, total_consumed, rate_limit_rps, rate_limit_burst, created_at, updated_at
			) VALUES ('root', 'Vault Root Admin', ?, ?, 'admin', 1, 1, 0, 0, 2000, 4000, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				token_hash = excluded.token_hash,
				token_prefix = excluded.token_prefix,
				updated_at = excluded.updated_at`,
			tokenHashHex, tokenPrefix, now, now,
		)
		return err
	})
}

// ListAccounts loads all API accounts from ReadDB.
func (s *Store) ListAccounts(ctx context.Context) ([]hotstate.AccountRecord, error) {
	rows, err := s.db.ReadDB.QueryContext(ctx, `
		SELECT id, name, token_hash, token_prefix, role, enabled, unlimited_quota,
		       balance_micros, total_consumed, rate_limit_rps, rate_limit_burst, created_at, updated_at
		FROM api_accounts
		ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]hotstate.AccountRecord, 0)
	for rows.Next() {
		var r hotstate.AccountRecord
		var enabledInt, unlimitedInt int
		if err := rows.Scan(
			&r.ID, &r.Name, &r.TokenHashHex, &r.TokenPrefix, &r.Role,
			&enabledInt, &unlimitedInt, &r.BalanceMicros, &r.TotalConsumed,
			&r.RateLimitRPS, &r.RateLimitBurst, &r.CreatedAt, &r.UpdatedAt,
		); err != nil {
			return nil, err
		}
		r.Enabled = enabledInt != 0
		r.UnlimitedQuota = unlimitedInt != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// CreateAccount inserts a new API account record on WriteDB.
func (s *Store) CreateAccount(ctx context.Context, rec hotstate.AccountRecord) (hotstate.AccountRecord, error) {
	now := skill.ISONow()
	if rec.ID == "" {
		rec.ID = skill.GenerateID("acct")
	}
	if rec.Role == "" {
		rec.Role = "device"
	}
	if rec.RateLimitRPS <= 0 {
		rec.RateLimitRPS = 50
	}
	if rec.RateLimitBurst <= 0 {
		rec.RateLimitBurst = 100
	}
	rec.CreatedAt = now
	rec.UpdatedAt = now

	enabledInt := 0
	if rec.Enabled {
		enabledInt = 1
	}
	unlimitedInt := 0
	if rec.UnlimitedQuota {
		unlimitedInt = 1
	}

	err := s.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO api_accounts (
				id, name, token_hash, token_prefix, role, enabled, unlimited_quota,
				balance_micros, total_consumed, rate_limit_rps, rate_limit_burst, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			rec.ID, rec.Name, rec.TokenHashHex, rec.TokenPrefix, rec.Role,
			enabledInt, unlimitedInt, rec.BalanceMicros, rec.TotalConsumed,
			rec.RateLimitRPS, rec.RateLimitBurst, rec.CreatedAt, rec.UpdatedAt,
		)
		return err
	})
	return rec, err
}

// TopUpAccountDB credits balance on WriteDB to keep durable storage aligned with in-memory TopUpAccount.
func (s *Store) TopUpAccountDB(ctx context.Context, accountID string, deltaMicros int64, requestID string, balanceAfter int64) error {
	now := skill.ISONow()
	return s.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE api_accounts SET balance_micros = balance_micros + ?, updated_at = ? WHERE id = ?`,
			deltaMicros, now, accountID,
		); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO quota_ledger (account_id, request_id, route, delta_micros, balance_after, created_at)
			VALUES (?, ?, 'POST /api/accounts/topup', ?, ?, ?)`,
			accountID, requestID, deltaMicros, balanceAfter, now,
		)
		return err
	})
}

// DeleteAccountDB removes a non-root API account on WriteDB.
func (s *Store) DeleteAccountDB(ctx context.Context, accountID string) (bool, error) {
	if accountID == "root" {
		return false, nil
	}
	deleted := false
	err := s.db.WithWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM api_accounts WHERE id = ?`, accountID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		deleted = n > 0
		return nil
	})
	return deleted, err
}

// ListAuditLogs reads latest async-committed access logs from ReadDB.
func (s *Store) ListAuditLogs(ctx context.Context, limit int) ([]AccessLogRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.ReadDB.QueryContext(ctx, `
		SELECT id, request_id, account_id, method, path, route, status_code, latency_us, cost_micros, client_ip, created_at
		FROM access_logs
		ORDER BY id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]AccessLogRow, 0)
	for rows.Next() {
		var r AccessLogRow
		if err := rows.Scan(
			&r.ID, &r.RequestID, &r.AccountID, &r.Method, &r.Path,
			&r.Route, &r.StatusCode, &r.LatencyUs, &r.CostMicros, &r.ClientIP, &r.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListQuotaLedger reads latest quota ledger mutations from ReadDB.
func (s *Store) ListQuotaLedger(ctx context.Context, accountID string, limit int) ([]QuotaLedgerRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var rows *sql.Rows
	var err error
	if accountID != "" {
		rows, err = s.db.ReadDB.QueryContext(ctx, `
			SELECT id, account_id, request_id, route, delta_micros, balance_after, created_at
			FROM quota_ledger
			WHERE account_id = ?
			ORDER BY id DESC
			LIMIT ?`, accountID, limit)
	} else {
		rows, err = s.db.ReadDB.QueryContext(ctx, `
			SELECT id, account_id, request_id, route, delta_micros, balance_after, created_at
			FROM quota_ledger
			ORDER BY id DESC
			LIMIT ?`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]QuotaLedgerRow, 0)
	for rows.Next() {
		var r QuotaLedgerRow
		if err := rows.Scan(&r.ID, &r.AccountID, &r.RequestID, &r.Route, &r.DeltaMicros, &r.BalanceAfter, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func nullStrPtr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	v := ns.String
	return &v
}

func nullIntPtr(ni sql.NullInt64) *int {
	if !ni.Valid {
		return nil
	}
	v := int(ni.Int64)
	return &v
}

func strPtrToNull(p *string) sql.NullString {
	if p == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *p, Valid: true}
}

func strValToNull(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
