package diagnostic

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/Gentleman-Programming/engram/v2/internal/cloudconfig"
	"github.com/Gentleman-Programming/engram/v2/internal/store"
)

// seedLiveRemoteConfig writes a cloud.json with one per-project override and
// returns its live cloud@<id> key plus the key a rotated token left behind.
func seedLiveRemoteConfig(t *testing.T, dataDir string) (live, orphan string) {
	t.Helper()
	t.Setenv(cloudconfig.EnvCloudServer, "")
	t.Setenv(cloudconfig.EnvCloudToken, "")
	cc := &cloudconfig.Config{ServerURL: "https://global.example.test", Token: "global-token"}
	if err := cloudconfig.SetProjectRemote(cc, "alpha", "https://team.example.test", "team-token"); err != nil {
		t.Fatalf("set override: %v", err)
	}
	if err := cloudconfig.Save(dataDir, cc); err != nil {
		t.Fatalf("save: %v", err)
	}
	live = cloudconfig.RemoteStateKeyPrefix + cloudconfig.RemoteID("https://team.example.test", "team-token")
	orphan = cloudconfig.RemoteStateKeyPrefix + cloudconfig.RemoteID("https://team.example.test", "old-token")
	return live, orphan
}

func seedDiagnosticSyncStates(t *testing.T, s *store.Store, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if _, err := s.DB().Exec(`INSERT INTO sync_state (target_key, lifecycle, updated_at) VALUES (?, 'idle', datetime('now'))`, key); err != nil {
			t.Fatalf("seed %q: %v", key, err)
		}
	}
}

func closedSpaceFindings(t *testing.T, scope Scope) (CheckResult, map[string]Finding) {
	t.Helper()
	result, err := SyncTargetClosedSpaceCheck{}.Run(context.Background(), scope)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	byKey := map[string]Finding{}
	for _, finding := range result.Findings {
		key := evidenceTargetKey(t, finding)
		if _, dup := byKey[key]; dup {
			t.Fatalf("duplicate finding for %q: %+v", key, result.Findings)
		}
		byKey[key] = finding
	}
	return result, byKey
}

func evidenceTargetKey(t *testing.T, finding Finding) string {
	t.Helper()
	var evidence map[string]any
	if err := json.Unmarshal(finding.Evidence, &evidence); err != nil {
		t.Fatalf("evidence: %v", err)
	}
	key, _ := evidence["target_key"].(string)
	return key
}

func TestSyncTargetClosedSpaceCheckSeparatesLiveAndOrphanedRemoteState(t *testing.T) {
	s, cfg := newDiagnosticTestStoreWithConfig(t)
	live, orphan := seedLiveRemoteConfig(t, cfg.DataDir)
	seedDiagnosticSyncStates(t, s, live, orphan, "satellite:inert")

	result, findings := closedSpaceFindings(t, Scope{Store: s, DataDir: cfg.DataDir})
	if _, flagged := findings[live]; flagged {
		t.Fatalf("live remote state must not be a finding: %+v", result.Findings)
	}
	orphanFinding, ok := findings[orphan]
	if !ok || orphanFinding.ReasonCode != ReasonOrphanedRemoteSyncState || orphanFinding.Severity != SeverityWarning {
		t.Fatalf("orphan finding = %+v (ok=%v), want a warning with reason %q", orphanFinding, ok, ReasonOrphanedRemoteSyncState)
	}
	foreign, ok := findings["satellite:inert"]
	if !ok || foreign.ReasonCode != ReasonForeignSyncTarget || foreign.Severity != SeverityError {
		t.Fatalf("other foreign targets keep their error finding: %+v", result.Findings)
	}
	if len(findings) != 2 {
		t.Fatalf("findings = %+v, want only the orphan and the foreign target", result.Findings)
	}
}

func TestSyncTargetClosedSpaceCheckOnlyLiveRemoteStateIsOK(t *testing.T) {
	s, cfg := newDiagnosticTestStoreWithConfig(t)
	live, _ := seedLiveRemoteConfig(t, cfg.DataDir)
	seedDiagnosticSyncStates(t, s, live)

	result, findings := closedSpaceFindings(t, Scope{Store: s, DataDir: cfg.DataDir})
	if len(findings) != 0 || result.Result != StatusOK {
		t.Fatalf("result = %+v, want ok with no findings", result)
	}
}

func TestSyncTargetClosedSpaceCheckUnreadableCloudConfigReportsOneNote(t *testing.T) {
	s, cfg := newDiagnosticTestStoreWithConfig(t)
	if err := os.WriteFile(cloudconfig.Path(cfg.DataDir), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	seedDiagnosticSyncStates(t, s, "cloud@aaaaaaaaaaaa", "cloud@bbbbbbbbbbbb", "satellite:inert")

	result, err := SyncTargetClosedSpaceCheck{}.Run(context.Background(), Scope{Store: s, DataDir: cfg.DataDir})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	notes, foreign := 0, 0
	for _, finding := range result.Findings {
		switch finding.ReasonCode {
		case ReasonRemoteSyncStateUnvalidated:
			notes++
			if finding.Severity != SeverityInfo {
				t.Fatalf("unvalidated note severity = %q, want info", finding.Severity)
			}
		case ReasonForeignSyncTarget:
			foreign++
			if key := evidenceTargetKey(t, finding); key != "satellite:inert" {
				t.Fatalf("cloud@ state reported as foreign error without a readable config: %+v", finding)
			}
		default:
			t.Fatalf("unexpected finding: %+v", finding)
		}
	}
	if notes != 1 || foreign != 1 {
		t.Fatalf("findings = %+v, want one info note and one foreign error", result.Findings)
	}
}

func TestBuildRepairPlanUsesLiveRemoteKeysFromScope(t *testing.T) {
	s, cfg := newDiagnosticTestStoreWithConfig(t)
	live, orphan := seedLiveRemoteConfig(t, cfg.DataDir)
	seedDiagnosticSyncStates(t, s, live, orphan)
	if _, err := s.DB().Exec(`INSERT INTO sync_apply_deferred (sync_id, entity, payload, target_key) VALUES ('deferred-1', 'observation', '{}', ?)`, orphan); err != nil {
		t.Fatalf("seed deferred: %v", err)
	}
	scope := Scope{Store: s, DataDir: cfg.DataDir}
	report, err := NewRunner().RunOne(context.Background(), scope, CheckSyncTargetClosedSpace)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	plan, err := BuildRepairPlan(context.Background(), scope, report, CheckSyncTargetClosedSpace, RepairModeDryRun)
	if err != nil {
		t.Fatalf("BuildRepairPlan: %v", err)
	}
	if len(plan.TargetActions) != 1 {
		t.Fatalf("target actions = %+v, want only the orphan", plan.TargetActions)
	}
	action := plan.TargetActions[0]
	if action.TargetKey != orphan || action.Reason != ReasonOrphanedRemoteSyncState || !action.StateRemoved || action.RetainedMutations != 0 || action.DiscardedDeferred != 1 {
		t.Fatalf("orphan action = %+v", action)
	}
}

func TestBuildRepairPlanUnreadableCloudConfigSkipsRemoteState(t *testing.T) {
	s, cfg := newDiagnosticTestStoreWithConfig(t)
	if err := os.WriteFile(cloudconfig.Path(cfg.DataDir), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	seedDiagnosticSyncStates(t, s, "cloud@aaaaaaaaaaaa")
	scope := Scope{Store: s, DataDir: cfg.DataDir}
	plan, err := BuildRepairPlan(context.Background(), scope, Report{}, CheckSyncTargetClosedSpace, RepairModeDryRun)
	if err != nil {
		t.Fatalf("BuildRepairPlan: %v", err)
	}
	if len(plan.TargetActions) != 0 {
		t.Fatalf("unreadable cloud.json must plan no cloud@ cleanup: %+v", plan.TargetActions)
	}
	if len(plan.Skipped) != 1 || plan.Skipped[0].ReasonCode != ReasonCloudConfigUnreadable {
		t.Fatalf("skipped = %+v, want one %q entry", plan.Skipped, ReasonCloudConfigUnreadable)
	}
}
