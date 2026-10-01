package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Gentleman-Programming/engram/v2/internal/cloudconfig"
	"github.com/Gentleman-Programming/engram/v2/internal/store"
	engramsync "github.com/Gentleman-Programming/engram/v2/internal/sync"
)

const projectRemoteSecret = "super-secret-project-token"

func runCloudConfigCLI(t *testing.T, cfg store.Config, args ...string) (string, string, any) {
	t.Helper()
	withArgs(t, append([]string{"engram", "cloud", "config"}, args...)...)
	return captureOutputAndRecover(t, func() { cmdCloud(cfg) })
}

// catchUpCall records one project catch-up pull from a new remote.
type catchUpCall struct {
	serverURL, token, project string
}

// stubCatchUpImport replaces the catch-up pull network step so tests never
// reach a remote; every call is recorded and returns err.
func stubCatchUpImport(t *testing.T, err error) *[]catchUpCall {
	t.Helper()
	calls := &[]catchUpCall{}
	old := cloudCatchUpPull
	cloudCatchUpPull = func(_ *store.Store, serverURL, token, project string) (*engramsync.ImportResult, error) {
		*calls = append(*calls, catchUpCall{serverURL: serverURL, token: token, project: project})
		if err != nil {
			return nil, err
		}
		return &engramsync.ImportResult{ChunksImported: 2}, nil
	}
	t.Cleanup(func() { cloudCatchUpPull = old })
	return calls
}

func pendingMutationCount(t *testing.T, cfg store.Config, project string) int {
	t.Helper()
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.Close()
	pending, err := s.ListPendingSyncMutations(store.DefaultSyncTargetKey, 1000)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	n := 0
	for _, m := range pending {
		if m.Project == project {
			n++
		}
	}
	return n
}

func seedDeliveredProject(t *testing.T, cfg store.Config, project string) {
	t.Helper()
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.Close()
	if err := s.CreateSession(project+"-session", project, "/tmp/"+project); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := s.AddObservation(store.AddObservationParams{SessionID: project + "-session", Type: "decision", Title: "t", Content: "c", Project: project, Scope: "project"}); err != nil {
		t.Fatalf("add observation: %v", err)
	}
	if err := s.EnrollProject(project); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	pending, err := s.ListPendingSyncMutations(store.DefaultSyncTargetKey, 1000)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	var maxSeq int64
	for _, m := range pending {
		if m.Seq > maxSeq {
			maxSeq = m.Seq
		}
	}
	if err := s.AckSyncMutations(store.DefaultSyncTargetKey, maxSeq); err != nil {
		t.Fatalf("ack: %v", err)
	}
}

func TestCmdCloudConfigProjectSetWritesOverrideAndKeepsGlobal(t *testing.T) {
	stubExitWithPanic(t)
	cfg := testConfig(t)
	if err := saveCloudConfig(cfg, &cloudConfig{ServerURL: "https://global.example.test", Token: "global-token"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	stdout, stderr, recovered := runCloudConfigCLI(t, cfg, "--project", " My-Project ", "--server", "https://team.example.test/", "--token", projectRemoteSecret)
	if recovered != nil {
		t.Fatalf("set override exited: %v stderr=%q", recovered, stderr)
	}
	if strings.Contains(stdout+stderr, projectRemoteSecret) {
		t.Fatalf("token printed in full: stdout=%q stderr=%q", stdout, stderr)
	}
	persisted, err := loadCloudConfig(cfg)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if persisted.ServerURL != "https://global.example.test" || persisted.Token != "global-token" {
		t.Fatalf("global fields changed: %+v", persisted)
	}
	override, ok := persisted.Projects["my-project"]
	if !ok {
		t.Fatalf("override not stored under normalized name: %+v", persisted.Projects)
	}
	if override.ServerURL != "https://team.example.test/" || override.Token != projectRemoteSecret {
		t.Fatalf("override = %+v", override)
	}
	if !strings.Contains(stdout, "my-project") || !strings.Contains(stdout, "https://team.example.test/") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestCmdCloudConfigProjectRejectsBadInput(t *testing.T) {
	cases := map[string][]string{
		"bad url":        {"--project", "p", "--server", "ftp://nope.example.test"},
		"missing server": {"--project", "p", "--token", "x"},
		"missing name":   {"--project", "  ", "--server", "https://ok.example.test"},
		"unknown flag":   {"--project", "p", "--server", "https://ok.example.test", "--bogus"},
		"clear + server": {"--project", "p", "--clear", "--server", "https://ok.example.test"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			stubExitWithPanic(t)
			cfg := testConfig(t)
			if err := saveCloudConfig(cfg, &cloudConfig{ServerURL: "https://global.example.test"}); err != nil {
				t.Fatalf("seed: %v", err)
			}
			_, _, recovered := runCloudConfigCLI(t, cfg, args...)
			if recovered == nil {
				t.Fatalf("expected exit for %v", args)
			}
			persisted, err := loadCloudConfig(cfg)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if len(persisted.Projects) != 0 || persisted.ServerURL != "https://global.example.test" {
				t.Fatalf("config mutated on bad input: %+v", persisted)
			}
		})
	}
}

func TestCmdCloudConfigProjectClearRemovesOnlyOverride(t *testing.T) {
	stubExitWithPanic(t)
	cfg := testConfig(t)
	seed := &cloudConfig{ServerURL: "https://global.example.test", Token: "global-token"}
	_ = cloudconfig.SetProjectRemote(seed, "a", "https://a.example.test", "ta")
	_ = cloudconfig.SetProjectRemote(seed, "b", "https://b.example.test", "tb")
	if err := saveCloudConfig(cfg, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	stdout, stderr, recovered := runCloudConfigCLI(t, cfg, "--project", "a", "--clear")
	if recovered != nil {
		t.Fatalf("clear exited: %v stderr=%q", recovered, stderr)
	}
	persisted, err := loadCloudConfig(cfg)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := persisted.Projects["a"]; ok {
		t.Fatalf("override a still present: %+v", persisted.Projects)
	}
	if _, ok := persisted.Projects["b"]; !ok {
		t.Fatalf("override b removed: %+v", persisted.Projects)
	}
	if persisted.ServerURL != "https://global.example.test" || persisted.Token != "global-token" {
		t.Fatalf("global fields changed: %+v", persisted)
	}
	if !strings.Contains(stdout, "global") {
		t.Fatalf("stdout = %q, want mention of global remote", stdout)
	}

	stdout, _, recovered = runCloudConfigCLI(t, cfg, "--project", "a", "--clear")
	if recovered != nil || !strings.Contains(stdout, "no cloud remote override") {
		t.Fatalf("clearing a missing override: stdout=%q panic=%v", stdout, recovered)
	}
}

func TestCmdCloudConfigProjectReassignmentRequeuesHistory(t *testing.T) {
	stubExitWithPanic(t)
	t.Setenv(cloudconfig.EnvCloudServer, "")
	t.Setenv(cloudconfig.EnvCloudToken, "")
	cfg := testConfig(t)
	if err := saveCloudConfig(cfg, &cloudConfig{ServerURL: "https://global.example.test", Token: "global-token"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	seedDeliveredProject(t, cfg, "moved")
	seedDeliveredProject(t, cfg, "stays")
	calls := stubCatchUpImport(t, nil)
	if pendingMutationCount(t, cfg, "moved") != 0 {
		t.Fatalf("precondition: moved has pending rows")
	}

	stdout, stderr, recovered := runCloudConfigCLI(t, cfg, "--project", "moved", "--server", "https://team.example.test", "--token", "tt")
	if recovered != nil {
		t.Fatalf("set exited: %v stderr=%q", recovered, stderr)
	}
	queued := pendingMutationCount(t, cfg, "moved")
	if queued == 0 || !strings.Contains(stdout, "Effective remote changed") {
		t.Fatalf("reassignment did not requeue history: pending=%d stdout=%q", queued, stdout)
	}
	if pendingMutationCount(t, cfg, "stays") != 0 {
		t.Fatalf("other project was requeued")
	}
	if len(*calls) != 1 || (*calls)[0] != (catchUpCall{serverURL: "https://team.example.test", token: "tt", project: "moved"}) {
		t.Fatalf("catch-up pull calls = %+v, want one pull of moved from the new remote", *calls)
	}
	if !strings.Contains(stdout, "Pulled 2 chunk(s)") {
		t.Fatalf("stdout should report the catch-up pull: %q", stdout)
	}

	// Same remote again is a no-op.
	stdout, _, recovered = runCloudConfigCLI(t, cfg, "--project", "moved", "--server", "https://team.example.test/", "--token", "tt")
	if recovered != nil || strings.Contains(stdout, "Effective remote changed") || !strings.Contains(stdout, "unchanged") {
		t.Fatalf("same remote should not requeue: stdout=%q panic=%v", stdout, recovered)
	}
	if got := pendingMutationCount(t, cfg, "moved"); got != queued {
		t.Fatalf("pending changed on same-remote set: %d -> %d", queued, got)
	}
	if len(*calls) != 1 {
		t.Fatalf("an unchanged remote must not pull again: %+v", *calls)
	}
}

func TestCmdCloudConfigProjectUnenrolledSkipsRequeue(t *testing.T) {
	stubExitWithPanic(t)
	cfg := testConfig(t)
	calls := stubCatchUpImport(t, nil)
	stdout, stderr, recovered := runCloudConfigCLI(t, cfg, "--project", "loose", "--server", "https://team.example.test", "--token", "tt")
	if recovered != nil {
		t.Fatalf("set exited: %v stderr=%q", recovered, stderr)
	}
	if !strings.Contains(stdout, "not enrolled") {
		t.Fatalf("stdout = %q, want not-enrolled note", stdout)
	}
	if len(*calls) != 0 {
		t.Fatalf("an unenrolled project must not pull: %+v", *calls)
	}
}

func TestCmdCloudConfigProjectCatchUpFailureKeepsConfigAndExitsZero(t *testing.T) {
	stubExitWithPanic(t)
	t.Setenv(cloudconfig.EnvCloudServer, "")
	t.Setenv(cloudconfig.EnvCloudToken, "")
	cfg := testConfig(t)
	if err := saveCloudConfig(cfg, &cloudConfig{ServerURL: "https://global.example.test", Token: "global-token"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	seedDeliveredProject(t, cfg, "moved")
	calls := stubCatchUpImport(t, errors.New("dial tcp: connection refused"))

	stdout, stderr, recovered := runCloudConfigCLI(t, cfg, "--project", "moved", "--server", "https://team.example.test", "--token", "tt")
	if recovered != nil {
		t.Fatalf("a failed catch-up pull must not exit non-zero: %v stderr=%q", recovered, stderr)
	}
	if len(*calls) != 1 {
		t.Fatalf("catch-up pull calls = %+v, want 1", *calls)
	}
	if !strings.Contains(stderr, "connection refused") || !strings.Contains(stderr, "engram sync --cloud --import --project moved") {
		t.Fatalf("stderr must warn with the manual retry command: %q", stderr)
	}
	if !strings.Contains(stdout, "Effective remote changed") || pendingMutationCount(t, cfg, "moved") == 0 {
		t.Fatalf("requeue must still have happened: stdout=%q", stdout)
	}
	cc, err := cloudconfig.Load(cfg.DataDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got, routed := cc.Projects["moved"]; !routed || got.ServerURL != "https://team.example.test" {
		t.Fatalf("routing must not be rolled back after a failed pull: %+v", cc.Projects)
	}
}

func TestCmdCloudConfigProjectWithoutTokenWarnsAutosyncSkips(t *testing.T) {
	stubExitWithPanic(t)
	t.Setenv(cloudconfig.EnvCloudServer, "")
	t.Setenv(cloudconfig.EnvCloudToken, "")
	cfg := testConfig(t)
	stubCatchUpImport(t, nil)
	stdout, stderr, recovered := runCloudConfigCLI(t, cfg, "--project", "loose", "--server", "https://team.example.test")
	if recovered != nil {
		t.Fatalf("set exited: %v stderr=%q", recovered, stderr)
	}
	out := stdout + stderr
	if !strings.Contains(out, `autosync will skip project "loose"`) || !strings.Contains(out, "engram cloud config --project loose --server https://team.example.test --token <token>") {
		t.Fatalf("missing tokenless-override warning: stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestPreflightCloudSyncRejectsTokenlessOverride(t *testing.T) {
	cfg := testConfig(t)
	t.Setenv(cloudconfig.EnvCloudServer, "")
	t.Setenv(cloudconfig.EnvCloudToken, "")
	seed := &cloudConfig{ServerURL: "https://global.example.test", Token: "global-token"}
	_ = cloudconfig.SetProjectRemote(seed, "notoken", "https://team.example.test", "")
	if err := saveCloudConfig(cfg, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.Close()
	if err := s.EnrollProject("notoken"); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	_, err = preflightCloudSync(s, cfg, "notoken", false)
	if err == nil || !strings.Contains(err.Error(), "engram cloud config --project notoken --server <url> --token <token>") {
		t.Fatalf("preflight err = %v, want an actionable tokenless-override error", err)
	}
}

func TestCmdCloudStatusListsProjectRemotes(t *testing.T) {
	stubExitWithPanic(t)
	t.Setenv(cloudconfig.EnvCloudServer, "")
	t.Setenv(cloudconfig.EnvCloudToken, "")
	cfg := testConfig(t)
	seed := &cloudConfig{}
	_ = cloudconfig.SetProjectRemote(seed, "alpha", "https://alpha.example.test", projectRemoteSecret)
	_ = cloudconfig.SetProjectRemote(seed, "beta", "https://beta.example.test", "")
	if err := saveCloudConfig(cfg, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	withArgs(t, "engram", "cloud", "status")
	stdout, stderr, recovered := captureOutputAndRecover(t, func() { cmdCloud(cfg) })
	if recovered != nil {
		t.Fatalf("status exited: %v stderr=%q", recovered, stderr)
	}
	if strings.Contains(stdout, projectRemoteSecret) {
		t.Fatalf("status printed token in full: %q", stdout)
	}
	alphaID := cloudconfig.RemoteID("https://alpha.example.test", projectRemoteSecret)
	for _, want := range []string{"Project remotes:", "alpha", "https://alpha.example.test", alphaID, "beta", "https://beta.example.test", "token not set"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("status missing %q: %q", want, stdout)
		}
	}
}

func TestPreflightCloudSyncUsesProjectOverride(t *testing.T) {
	cfg := testConfig(t)
	t.Setenv(cloudconfig.EnvCloudServer, "https://env-global.example.test")
	t.Setenv(cloudconfig.EnvCloudToken, "env-global-token")
	seed := &cloudConfig{ServerURL: "https://global.example.test", Token: "global-token"}
	_ = cloudconfig.SetProjectRemote(seed, "routed", "https://routed.example.test", "routed-token")
	if err := saveCloudConfig(cfg, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.Close()
	for _, p := range []string{"routed", "plain"} {
		if err := s.EnrollProject(p); err != nil {
			t.Fatalf("enroll %s: %v", p, err)
		}
	}

	routed, err := preflightCloudSync(s, cfg, "Routed", false)
	if err != nil {
		t.Fatalf("preflight routed: %v", err)
	}
	if routed.ServerURL != "https://routed.example.test" || routed.Token != "routed-token" {
		t.Fatalf("routed runtime config = %+v", routed)
	}
	plain, err := preflightCloudSync(s, cfg, "plain", false)
	if err != nil {
		t.Fatalf("preflight plain: %v", err)
	}
	if plain.ServerURL != "https://env-global.example.test" || plain.Token != "env-global-token" {
		t.Fatalf("plain runtime config = %+v", plain)
	}
}

func TestPreflightCloudSyncRejectsMalformedOverride(t *testing.T) {
	cfg := testConfig(t)
	raw := `{"server_url":"https://global.example.test","projects":{"broken":{"server_url":""}}}`
	if err := os.WriteFile(cloudconfig.Path(cfg.DataDir), []byte(raw), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.Close()
	if err := s.EnrollProject("broken"); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if _, err := preflightCloudSync(s, cfg, "broken", false); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("preflight err = %v, want malformed override error", err)
	}
}

func TestCmdCloudUpgradeRemirrorUsesProjectOverride(t *testing.T) {
	cfg := testConfig(t)
	t.Setenv(cloudconfig.EnvCloudServer, "")
	t.Setenv(cloudconfig.EnvCloudToken, "")
	seed := &cloudConfig{ServerURL: "https://global.example.test", Token: "global-token"}
	_ = cloudconfig.SetProjectRemote(seed, "routed", "https://routed.example.test", "routed-token")
	if err := saveCloudConfig(cfg, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var got *cloudconfig.Config
	oldRun := runUpgradeRemirror
	runUpgradeRemirror = func(_ *store.Store, _ string, cc *cloudconfig.Config) (*engramsync.SyncResult, error) {
		got = cc
		return &engramsync.SyncResult{}, nil
	}
	t.Cleanup(func() { runUpgradeRemirror = oldRun })

	withArgs(t, "engram", "cloud", "upgrade", "remirror", "--project", "routed")
	_, stderr, recovered := captureOutputAndRecover(t, func() { cmdCloudUpgradeRemirror(cfg) })
	if recovered != nil {
		t.Fatalf("remirror exited: %v stderr=%q", recovered, stderr)
	}
	if got == nil || got.ServerURL != "https://routed.example.test" || got.Token != "routed-token" {
		t.Fatalf("remirror config = %+v, want routed override", got)
	}
}

func TestCmdCloudConfigProjectClearBackToGlobalRequeuesHistory(t *testing.T) {
	stubExitWithPanic(t)
	t.Setenv(cloudconfig.EnvCloudServer, "")
	t.Setenv(cloudconfig.EnvCloudToken, "")
	cfg := testConfig(t)
	seed := &cloudConfig{ServerURL: "https://global.example.test", Token: "global-token"}
	_ = cloudconfig.SetProjectRemote(seed, "routed", "https://team.example.test", "tt")
	if err := saveCloudConfig(cfg, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	seedDeliveredProject(t, cfg, "routed")
	calls := stubCatchUpImport(t, nil)

	stdout, stderr, recovered := runCloudConfigCLI(t, cfg, "--project", "routed", "--clear")
	if recovered != nil {
		t.Fatalf("clear exited: %v stderr=%q", recovered, stderr)
	}
	if !strings.Contains(stdout, "Effective remote changed") || pendingMutationCount(t, cfg, "routed") == 0 {
		t.Fatalf("clearing back to global did not requeue: stdout=%q", stdout)
	}
	if len(*calls) != 1 || (*calls)[0] != (catchUpCall{serverURL: "https://global.example.test", token: "global-token", project: "routed"}) {
		t.Fatalf("clear must catch up from the global remote: %+v", *calls)
	}
}
