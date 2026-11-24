package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"

	"skillvault"
	"skillvault/internal/db"
	"skillvault/internal/hotstate"
	"skillvault/internal/skill"
	"skillvault/internal/store"
)

func TestStoreReadWriteSplitAndLifecycle(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "store.db")

	sdb, err := db.Open(ctx, db.Options{
		Path:        dbPath,
		ReadConns:   5,
		MigrationFS: skillvault.MigrationFiles,
	})
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer sdb.Close()

	st := store.New(sdb)

	v1 := map[string]skill.FileEntry{
		"SKILL.md": {Encoding: "utf8", Content: "# Helper v1\n", Mode: "0644"},
	}
	h1, err := skill.ComputeFilesHash(v1)
	if err != nil {
		t.Fatal(err)
	}
	sk, err := st.CreateSkillWithVersion(ctx, store.CreateSkillInput{
		ID:          "helper",
		Name:        "Helper",
		Description: "Test helper",
		SourceType:  "custom",
		Note:        "v1",
		Files:       v1,
		ContentHash: h1,
	})
	if err != nil {
		t.Fatalf("CreateSkillWithVersion failed: %v", err)
	}
	if sk.CurrentVersion == nil || sk.CurrentVersion.VersionNum != 1 {
		t.Fatalf("expected v1, got %+v", sk.CurrentVersion)
	}

	dev, err := st.UpsertDevice(ctx, "dev-1", "DevBox", "linux", "amd64", nil, []store.DetectedAgent{
		{ID: "codex", Name: "Codex", SkillsDir: "~/.codex/skills", Exists: true},
	})
	if err != nil || dev == nil {
		t.Fatalf("UpsertDevice failed: %v", err)
	}

	vaultID := "helper"
	if err := st.ReplaceDeviceInstallations(ctx, "dev-1", []store.ScannedSkillInput{
		{
			AgentID:      "codex",
			SkillName:    "helper",
			VaultSkillID: &vaultID,
			BaseHash:     &h1,
			ActualHash:   &h1,
		},
	}); err != nil {
		t.Fatalf("ReplaceDeviceInstallations failed: %v", err)
	}

	// Add v2 -> linked installation must automatically transition to outdated in the same WriteDB tx.
	v2 := map[string]skill.FileEntry{
		"SKILL.md": {Encoding: "utf8", Content: "# Helper v2\n", Mode: "0644"},
	}
	h2, err := skill.ComputeFilesHash(v2)
	if err != nil {
		t.Fatal(err)
	}
	ver2, err := st.AddSkillVersion(ctx, "helper", v2, h2, nil, "v2")
	if err != nil || ver2.VersionNum != 2 {
		t.Fatalf("AddSkillVersion failed: %v (ver=%+v)", err, ver2)
	}

	inst, err := st.GetInstallation(ctx, "dev-1", "codex", "helper", false)
	if err != nil || inst == nil {
		t.Fatalf("GetInstallation failed: %v", err)
	}
	if inst.Status != skill.InstallStatusOutdated {
		t.Fatalf("status = %q, want outdated", inst.Status)
	}

	// Create update action and verify ClaimPendingActions hydrates version files.
	act, err := st.CreateAction(ctx, "dev-1", "codex", "helper", skill.ActionUpdate, map[string]any{
		"vaultSkillId": "helper",
		"versionId":    ver2.ID,
	})
	if err != nil || act == nil {
		t.Fatalf("CreateAction failed: %v", err)
	}
	claimed, err := st.ClaimPendingActions(ctx, "dev-1")
	if err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimPendingActions failed: %v len=%d", err, len(claimed))
	}
	filesMap, ok := claimed[0].Payload["files"].(map[string]skill.FileEntry)
	if !ok || filesMap["SKILL.md"].Content != "# Helper v2\n" {
		t.Fatalf("expected hydrated v2 files in claimed payload, got %#v", claimed[0].Payload["files"])
	}

	// Account & SystemConfig persistence round-trip.
	sum := sha256.Sum256([]byte("svk_store_test"))
	rec, err := st.CreateAccount(ctx, hotstate.AccountRecord{
		Name:           "StoreAcct",
		TokenHashHex:   hex.EncodeToString(sum[:]),
		TokenPrefix:    "svk_store",
		BalanceMicros:  500,
		Enabled:        true,
		RateLimitRPS:   100,
		RateLimitBurst: 200,
	})
	if err != nil {
		t.Fatalf("CreateAccount failed: %v", err)
	}
	accounts, err := st.ListAccounts(ctx)
	if err != nil || len(accounts) != 1 || accounts[0].ID != rec.ID {
		t.Fatalf("ListAccounts mismatch: %+v err=%v", accounts, err)
	}
}
