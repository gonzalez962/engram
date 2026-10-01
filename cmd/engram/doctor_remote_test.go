package main

import (
	"os"
	"testing"

	"github.com/Gentleman-Programming/engram/v2/internal/cloudconfig"
	"github.com/Gentleman-Programming/engram/v2/internal/store"
)

func seedDoctorRemoteStates(t *testing.T, cfg store.Config, keys ...string) {
	t.Helper()
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.Close()
	if err := s.EnrollProject("valid"); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	for _, key := range keys {
		if _, err := s.DB().Exec(`INSERT INTO sync_state (target_key, lifecycle, last_pulled_seq, updated_at) VALUES (?, 'idle', 9, datetime('now'))`, key); err != nil {
			t.Fatalf("seed %q: %v", key, err)
		}
	}
}

func doctorSyncStateExists(t *testing.T, cfg store.Config, key string) bool {
	t.Helper()
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.Close()
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM sync_state WHERE target_key = ?`, key).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n == 1
}

func runDoctorTargetRepairApply(t *testing.T, cfg store.Config) map[string]any {
	t.Helper()
	withArgs(t, "engram", "doctor", "repair", "--project", "valid", "--check", "sync_target_closed_space", "--apply")
	stdout, stderr := captureOutput(t, func() { cmdDoctor(cfg) })
	if stderr != "" {
		t.Fatalf("doctor stderr=%q", stderr)
	}
	return decodeRepairPlan(t, stdout)
}

func TestCmdDoctorRepairKeepsLiveRemoteStateAndPrunesOrphans(t *testing.T) {
	t.Setenv(cloudconfig.EnvCloudServer, "")
	t.Setenv(cloudconfig.EnvCloudToken, "")
	cfg := testConfig(t)
	cc := &cloudconfig.Config{ServerURL: "https://global.example.test", Token: "global-token"}
	if err := cloudconfig.SetProjectRemote(cc, "alpha", "https://team.example.test", "team-token"); err != nil {
		t.Fatalf("set override: %v", err)
	}
	if err := cloudconfig.Save(cfg.DataDir, cc); err != nil {
		t.Fatalf("save: %v", err)
	}
	live := cloudconfig.RemoteStateKeyPrefix + cloudconfig.RemoteID("https://team.example.test", "team-token")
	// A rotated token leaves the previous remote id behind.
	orphan := cloudconfig.RemoteStateKeyPrefix + cloudconfig.RemoteID("https://team.example.test", "old-token")
	seedDoctorRemoteStates(t, cfg, live, orphan)

	plan := runDoctorTargetRepairApply(t, cfg)
	if plan["status"] != "applied" {
		t.Fatalf("plan=%v", plan)
	}
	actions, _ := plan["target_actions"].([]any)
	if len(actions) != 1 {
		t.Fatalf("target_actions=%v, want only the orphan", actions)
	}
	action := actions[0].(map[string]any)
	if action["target_key"] != orphan || action["state_removed"] != true || action["reason"] != "orphaned_remote_state" {
		t.Fatalf("orphan action=%v", action)
	}
	if !doctorSyncStateExists(t, cfg, live) {
		t.Fatal("doctor deleted live cloud@ state")
	}
	if doctorSyncStateExists(t, cfg, orphan) {
		t.Fatal("doctor kept orphaned cloud@ state")
	}
}

func TestCmdDoctorRepairUnreadableCloudConfigPrunesNoRemoteState(t *testing.T) {
	cfg := testConfig(t)
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(cloudconfig.Path(cfg.DataDir), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	const remoteKey = "cloud@aaaaaaaaaaaa"
	seedDoctorRemoteStates(t, cfg, remoteKey, "satellite:stale")

	plan := runDoctorTargetRepairApply(t, cfg)
	if !doctorSyncStateExists(t, cfg, remoteKey) {
		t.Fatal("unreadable cloud.json must prune nothing under cloud@")
	}
	if doctorSyncStateExists(t, cfg, "satellite:stale") {
		t.Fatal("legacy foreign cleanup must still run")
	}
	skipped, _ := plan["skipped"].([]any)
	found := false
	for _, entry := range skipped {
		if entry.(map[string]any)["reason_code"] == "cloud_config_unreadable" {
			found = true
		}
	}
	if !found {
		t.Fatalf("plan must report the unreadable cloud config: %v", plan)
	}
}
