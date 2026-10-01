package diagnostic

import (
	"context"
	"os"
	"testing"

	"github.com/Gentleman-Programming/engram/v2/internal/cloudconfig"
)

func TestLiveCloudRemoteStateKeysListsEveryConfiguredOverride(t *testing.T) {
	t.Setenv(cloudconfig.EnvCloudServer, "")
	t.Setenv(cloudconfig.EnvCloudToken, "")
	dir := t.TempDir()
	cc := &cloudconfig.Config{ServerURL: "https://global.example.test", Token: "global-token"}
	if err := cloudconfig.SetProjectRemote(cc, "alpha", "https://team.example.test", "team-token"); err != nil {
		t.Fatalf("set alpha: %v", err)
	}
	if err := cloudconfig.SetProjectRemote(cc, "beta", "https://team.example.test", "team-token"); err != nil {
		t.Fatalf("set beta: %v", err)
	}
	if err := cloudconfig.Save(dir, cc); err != nil {
		t.Fatalf("save: %v", err)
	}

	keys, err := LiveCloudRemoteStateKeys(dir)
	if err != nil {
		t.Fatalf("LiveCloudRemoteStateKeys: %v", err)
	}
	want := cloudconfig.RemoteStateKeyPrefix + cloudconfig.RemoteID("https://team.example.test", "team-token")
	if len(keys) != 1 || !keys[want] {
		t.Fatalf("keys = %v, want only %q", keys, want)
	}
}

func TestLiveCloudRemoteStateKeysWithoutOverridesIsEmptyNotNil(t *testing.T) {
	keys, err := LiveCloudRemoteStateKeys(t.TempDir())
	if err != nil {
		t.Fatalf("LiveCloudRemoteStateKeys: %v", err)
	}
	if keys == nil || len(keys) != 0 {
		t.Fatalf("keys = %#v, want an empty non-nil set (every cloud@ row is an orphan)", keys)
	}
}

func TestLiveCloudRemoteStateKeysUnreadableConfigFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(cloudconfig.Path(dir), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	keys, err := LiveCloudRemoteStateKeys(dir)
	if err == nil || keys != nil {
		t.Fatalf("keys=%v err=%v, want nil keys and an error", keys, err)
	}
}

func TestBuildRepairPlanNeverListsRemoteStateWithoutConfig(t *testing.T) {
	s := newDiagnosticTestStore(t)
	if _, err := s.DB().Exec(`INSERT INTO sync_state (target_key, lifecycle, updated_at) VALUES ('cloud@aaaaaaaaaaaa', 'idle', datetime('now'))`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	report, err := NewRunner().RunOne(context.Background(), Scope{Store: s}, CheckSyncTargetClosedSpace)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	plan, err := BuildRepairPlan(context.Background(), Scope{Store: s}, report, CheckSyncTargetClosedSpace, RepairModeDryRun)
	if err != nil {
		t.Fatalf("BuildRepairPlan: %v", err)
	}
	for _, action := range plan.TargetActions {
		if action.TargetKey == "cloud@aaaaaaaaaaaa" {
			t.Fatalf("repair plan without configured remotes must keep cloud@ state: %+v", plan.TargetActions)
		}
	}
}
