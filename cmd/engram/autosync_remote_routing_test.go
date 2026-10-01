package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Gentleman-Programming/engram/v2/internal/cloud/autosync"
	"github.com/Gentleman-Programming/engram/v2/internal/cloudconfig"
	"github.com/Gentleman-Programming/engram/v2/internal/server"
	"github.com/Gentleman-Programming/engram/v2/internal/store"
)

// routingFakeManager records the config it was built with and the upgrade
// pause/resume calls routed to it.
type routingFakeManager struct {
	cfg     autosync.Config
	phase   string
	mu      sync.Mutex
	paused  []string
	resumed []string
	dirty   int
	stopped int
}

func (f *routingFakeManager) Run(context.Context) {}
func (f *routingFakeManager) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped++
}
func (f *routingFakeManager) Status() autosync.Status {
	return autosync.Status{Phase: f.phase, ReasonMessage: f.cfg.StateKey}
}
func (f *routingFakeManager) NotifyDirty() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirty++
}
func (f *routingFakeManager) StopForUpgrade(project string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = append(f.paused, project)
	return nil
}
func (f *routingFakeManager) ResumeAfterUpgrade(project string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumed = append(f.resumed, project)
	return nil
}

func captureAutosyncManagers(t *testing.T) *[]*routingFakeManager {
	t.Helper()
	built := &[]*routingFakeManager{}
	old := newAutosyncManager
	newAutosyncManager = func(_ *store.Store, _ autosync.CloudTransport, cfg autosync.Config) startableAutosyncManager {
		fake := &routingFakeManager{cfg: cfg, phase: autosync.PhaseHealthy}
		*built = append(*built, fake)
		return fake
	}
	t.Cleanup(func() { newAutosyncManager = old })
	return built
}

func startRoutedAutosync(t *testing.T, cc *cloudconfig.Config) (store.Config, autosyncStatusProvider, func(), []*routingFakeManager) {
	t.Helper()
	cfg := testConfig(t)
	t.Setenv("ENGRAM_CLOUD_AUTOSYNC", "1")
	t.Setenv(cloudconfig.EnvCloudServer, "")
	t.Setenv(cloudconfig.EnvCloudToken, "")
	if err := cloudconfig.Save(cfg.DataDir, cc); err != nil {
		t.Fatalf("save cloud config: %v", err)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	built := captureAutosyncManagers(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgr, stop := tryStartAutosync(ctx, s, cfg)
	if stop != nil {
		t.Cleanup(stop)
	}
	return cfg, mgr, stop, *built
}

func managerByStateKey(t *testing.T, managers []*routingFakeManager, key string) *routingFakeManager {
	t.Helper()
	for _, m := range managers {
		if m.cfg.StateKey == key {
			return m
		}
	}
	t.Fatalf("no manager with state key %q", key)
	return nil
}

func TestTryStartAutosyncLegacyConfigBuildsOneUnscopedManager(t *testing.T) {
	_, mgr, _, built := startRoutedAutosync(t, &cloudconfig.Config{ServerURL: "https://global.example.test", Token: "global-token"})
	if mgr == nil || len(built) != 1 {
		t.Fatalf("managers = %d (provider nil=%v), want exactly 1", len(built), mgr == nil)
	}
	got := built[0].cfg
	if got.StateKey != store.DefaultSyncTargetKey || got.TargetKey != store.DefaultSyncTargetKey {
		t.Fatalf("legacy keys = target %q state %q, want cloud/cloud", got.TargetKey, got.StateKey)
	}
	if got.IncludeProjects != nil || got.ExcludeProjects != nil {
		t.Fatalf("legacy manager must be unscoped, got include=%v exclude=%v", got.IncludeProjects, got.ExcludeProjects)
	}
}

func TestTryStartAutosyncBuildsOneManagerPerRemote(t *testing.T) {
	shared := cloudconfig.ProjectRemote{ServerURL: "https://team.example.test", Token: "team-token"}
	cc := &cloudconfig.Config{
		ServerURL: "https://global.example.test",
		Token:     "global-token",
		Projects: map[string]cloudconfig.ProjectRemote{
			"alpha":  shared,
			"beta":   shared,
			"gamma":  {ServerURL: "https://other.example.test", Token: "other-token"},
			"broken": {ServerURL: "not a url"},
		},
	}
	_, mgr, _, built := startRoutedAutosync(t, cc)
	if mgr == nil || len(built) != 3 {
		t.Fatalf("managers = %d, want global + 2 remotes (malformed skipped)", len(built))
	}

	global := managerByStateKey(t, built, store.DefaultSyncTargetKey)
	if got := strings.Join(global.cfg.ExcludeProjects, ","); got != "alpha,beta,broken,gamma" {
		t.Fatalf("global exclude = %q; routed and malformed projects must never use the global remote", got)
	}
	if global.cfg.IncludeProjects != nil {
		t.Fatalf("global include = %v, want nil", global.cfg.IncludeProjects)
	}

	teamKey := cloudconfig.RemoteStateKeyPrefix + cloudconfig.RemoteID("https://team.example.test", "team-token")
	team := managerByStateKey(t, built, teamKey)
	if got := strings.Join(team.cfg.IncludeProjects, ","); got != "alpha,beta" {
		t.Fatalf("shared remote include = %q, want alpha,beta", got)
	}
	otherKey := cloudconfig.RemoteStateKeyPrefix + cloudconfig.RemoteID("https://other.example.test", "other-token")
	other := managerByStateKey(t, built, otherKey)
	if got := strings.Join(other.cfg.IncludeProjects, ","); got != "gamma" {
		t.Fatalf("other remote include = %q, want gamma", got)
	}
	for _, m := range built {
		if m.cfg.TargetKey != store.DefaultSyncTargetKey {
			t.Fatalf("journal key = %q, want %q for every manager", m.cfg.TargetKey, store.DefaultSyncTargetKey)
		}
	}
	owners := map[string]bool{}
	for _, m := range built {
		owners[m.cfg.LeaseOwner] = true
	}
	if len(owners) != len(built) {
		t.Fatalf("lease owners must be distinct, got %v", owners)
	}
}

func TestTryStartAutosyncStartsOverridesWithoutGlobalServer(t *testing.T) {
	cc := &cloudconfig.Config{Projects: map[string]cloudconfig.ProjectRemote{
		"alpha": {ServerURL: "https://team.example.test", Token: "team-token"},
	}}
	_, mgr, stop, built := startRoutedAutosync(t, cc)
	if mgr == nil || stop == nil || len(built) != 1 {
		t.Fatalf("managers = %d, want the override manager only", len(built))
	}
	if built[0].cfg.StateKey == store.DefaultSyncTargetKey || strings.Join(built[0].cfg.IncludeProjects, ",") != "alpha" {
		t.Fatalf("unexpected manager cfg: %+v", built[0].cfg)
	}
}

func TestTryStartAutosyncOnlyMalformedOverrideAndNoGlobalStartsNothing(t *testing.T) {
	cc := &cloudconfig.Config{Projects: map[string]cloudconfig.ProjectRemote{"broken": {ServerURL: "::bad"}}}
	_, mgr, stop, built := startRoutedAutosync(t, cc)
	if mgr != nil || stop != nil || len(built) != 0 {
		t.Fatalf("expected no autosync, got %d managers", len(built))
	}
}

func TestAutosyncGroupUpgradePauseIsNoOpForUnownedProjectWithOverridesOnly(t *testing.T) {
	cc := &cloudconfig.Config{Projects: map[string]cloudconfig.ProjectRemote{
		"alpha": {ServerURL: "https://team.example.test", Token: "team-token"},
	}}
	_, mgr, _, built := startRoutedAutosync(t, cc)
	group, ok := mgr.(*autosyncGroup)
	if !ok || len(built) != 1 {
		t.Fatalf("provider = %T with %d managers, want one override manager", mgr, len(built))
	}
	if err := group.StopForUpgrade("unrouted"); err != nil {
		t.Fatalf("stop unrouted: %v", err)
	}
	if err := group.ResumeAfterUpgrade("unrouted"); err != nil {
		t.Fatalf("resume unrouted: %v", err)
	}
	if len(built[0].paused) != 0 || len(built[0].resumed) != 0 {
		t.Fatalf("unrouted project reached the override manager: paused=%v resumed=%v", built[0].paused, built[0].resumed)
	}
}

func TestTryStartAutosyncEnvOnlyGlobalStartsOneLegacyManager(t *testing.T) {
	cfg := testConfig(t)
	t.Setenv("ENGRAM_CLOUD_AUTOSYNC", "1")
	t.Setenv(cloudconfig.EnvCloudServer, "https://env-global.example.test")
	t.Setenv(cloudconfig.EnvCloudToken, "env-global-token")
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	built := captureAutosyncManagers(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgr, stop := tryStartAutosync(ctx, s, cfg)
	if stop != nil {
		t.Cleanup(stop)
	}
	if mgr == nil || stop == nil || len(*built) != 1 {
		t.Fatalf("managers = %d (provider nil=%v), want exactly one global manager", len(*built), mgr == nil)
	}
	got := (*built)[0].cfg
	if got.StateKey != store.DefaultSyncTargetKey || got.TargetKey != store.DefaultSyncTargetKey || got.IncludeProjects != nil || got.ExcludeProjects != nil {
		t.Fatalf("env-only manager cfg = %+v, want the unscoped legacy manager", got)
	}
}

func routedGroup(t *testing.T) (*autosyncGroup, *routingFakeManager, *routingFakeManager) {
	t.Helper()
	cc := &cloudconfig.Config{
		ServerURL: "https://global.example.test",
		Token:     "global-token",
		Projects: map[string]cloudconfig.ProjectRemote{
			"alpha":  {ServerURL: "https://team.example.test", Token: "team-token"},
			"broken": {ServerURL: "not a url"},
		},
	}
	_, mgr, _, built := startRoutedAutosync(t, cc)
	group, ok := mgr.(*autosyncGroup)
	if !ok {
		t.Fatalf("provider type = %T, want *autosyncGroup", mgr)
	}
	global := managerByStateKey(t, built, store.DefaultSyncTargetKey)
	var team *routingFakeManager
	for _, m := range built {
		if m != global {
			team = m
		}
	}
	global.phase = autosync.PhasePushFailed
	return group, global, team
}

func TestAutosyncGroupStopForUpgradeStopsOnlyOwner(t *testing.T) {
	group, global, team := routedGroup(t)
	if err := group.StopForUpgrade("alpha"); err != nil {
		t.Fatalf("stop alpha: %v", err)
	}
	if len(team.paused) != 1 || len(global.paused) != 0 {
		t.Fatalf("paused team=%v global=%v, want only the alpha owner", team.paused, global.paused)
	}
	if err := group.StopForUpgrade("unrouted"); err != nil {
		t.Fatalf("stop unrouted: %v", err)
	}
	if len(global.paused) != 1 || len(team.paused) != 1 {
		t.Fatalf("unrouted project must pause only the global manager: team=%v global=%v", team.paused, global.paused)
	}
	if err := group.ResumeAfterUpgrade("alpha"); err != nil || len(team.resumed) != 1 || len(global.resumed) != 0 {
		t.Fatalf("resume routed to wrong manager: err=%v team=%v global=%v", err, team.resumed, global.resumed)
	}
	// A project whose remote failed to start has no manager to pause; like the
	// legacy "no manager" path this is a no-op, not an error.
	if err := group.StopForUpgrade("broken"); err != nil {
		t.Fatalf("stop broken: %v", err)
	}
	if err := group.ResumeAfterUpgrade("broken"); err != nil {
		t.Fatalf("resume broken: %v", err)
	}
	if len(global.paused) != 1 || len(team.paused) != 1 {
		t.Fatalf("unowned project must pause nothing: team=%v global=%v", team.paused, global.paused)
	}
	group.NotifyDirty()
	if global.dirty != 1 || team.dirty != 1 {
		t.Fatalf("dirty fan-out global=%d team=%d, want 1/1", global.dirty, team.dirty)
	}
	group.Stop()
	if global.stopped == 0 || team.stopped == 0 {
		t.Fatalf("stop must reach every manager: global=%d team=%d", global.stopped, team.stopped)
	}
}

type stubStatusFallback struct{ calls []string }

func (s *stubStatusFallback) Status(project string) server.SyncStatus {
	s.calls = append(s.calls, project)
	return server.SyncStatus{Phase: "fallback", UpgradeStage: "stage-" + project}
}

func TestAutosyncStatusAdapterRoutesByProject(t *testing.T) {
	group, _, _ := routedGroup(t)
	fallback := &stubStatusFallback{}
	adapter := &autosyncStatusAdapter{mgr: group, fallback: fallback}

	if got := adapter.Status("alpha"); got.Phase != "healthy" || got.UpgradeStage != "stage-alpha" {
		t.Fatalf("alpha status = %+v, want the team manager's healthy phase", got)
	}
	if got := adapter.Status("unrouted"); got.Phase != "degraded" {
		t.Fatalf("unrouted status = %+v, want the global manager's degraded phase", got)
	}
	if got := adapter.Status("broken"); got.Phase != "fallback" {
		t.Fatalf("broken status = %+v, want store fallback (no manager owns it)", got)
	}
}

func TestStoreSyncStatusProviderResolvesCloudPerProject(t *testing.T) {
	cfg := testConfig(t)
	t.Setenv(cloudconfig.EnvCloudServer, "")
	t.Setenv(cloudconfig.EnvCloudToken, "")
	if err := cloudconfig.Save(cfg.DataDir, &cloudconfig.Config{Projects: map[string]cloudconfig.ProjectRemote{
		"alpha":  {ServerURL: "https://team.example.test", Token: "team-token"},
		"broken": {ServerURL: "not a url"},
	}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.Close()
	for _, project := range []string{"alpha", "beta", "broken"} {
		if err := s.EnrollProject(project); err != nil {
			t.Fatalf("enroll: %v", err)
		}
	}
	p := storeSyncStatusProvider{store: s, cfg: cfg}
	if ok, code, msg := p.cloudSyncEnabled("alpha"); !ok {
		t.Fatalf("overridden project must count as configured without a global server: %s %s", code, msg)
	}
	if ok, code, _ := p.cloudSyncEnabled("beta"); ok || code != "cloud_not_configured" {
		t.Fatalf("unrouted project without global server: ok=%v code=%q", ok, code)
	}
	if ok, code, _ := p.cloudSyncEnabled("broken"); ok || code != "cloud_config_error" {
		t.Fatalf("malformed override: ok=%v code=%q", ok, code)
	}
}

func TestCmdCloudConfigProjectRestoresConfigWhenRequeueFails(t *testing.T) {
	stubExitWithPanic(t)
	t.Setenv(cloudconfig.EnvCloudServer, "")
	t.Setenv(cloudconfig.EnvCloudToken, "")
	cfg := testConfig(t)
	if err := cloudconfig.Save(cfg.DataDir, &cloudconfig.Config{ServerURL: "https://global.example.test", Token: "global-token"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	seedDeliveredProject(t, cfg, "moved")

	stubCatchUpImport(t, nil)
	oldStoreNew := storeNew
	t.Cleanup(func() { storeNew = oldStoreNew })
	storeNew = func(store.Config) (*store.Store, error) { return nil, errors.New("database is locked") }
	_, stderr, recovered := runCloudConfigCLI(t, cfg, "--project", "moved", "--server", "https://team.example.test", "--token", "tt")
	storeNew = oldStoreNew
	if _, ok := recovered.(exitCode); !ok {
		t.Fatalf("expected exit on requeue failure, got %v stderr=%q", recovered, stderr)
	}
	restored, err := cloudconfig.Load(cfg.DataDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, routed := restored.Projects["moved"]; routed || restored.ServerURL != "https://global.example.test" || restored.Token != "global-token" {
		t.Fatalf("cloud.json not restored after requeue failure: %+v", restored)
	}
	if !strings.Contains(stderr, "restored") {
		t.Fatalf("stderr should say the previous config was restored: %q", stderr)
	}

	// Re-running retries the reassignment instead of reporting "unchanged".
	stdout, stderr, recovered := runCloudConfigCLI(t, cfg, "--project", "moved", "--server", "https://team.example.test", "--token", "tt")
	if recovered != nil {
		t.Fatalf("retry exited: %v stderr=%q", recovered, stderr)
	}
	if !strings.Contains(stdout, "Effective remote changed") || pendingMutationCount(t, cfg, "moved") == 0 {
		t.Fatalf("retry did not requeue: stdout=%q", stdout)
	}
	if !strings.Contains(strings.ToLower(stdout), "restart") {
		t.Fatalf("config change must tell the user to restart serve/mcp: %q", stdout)
	}
}
