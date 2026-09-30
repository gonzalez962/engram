package autosync

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/Gentleman-Programming/engram/v2/internal/store"
)

// routingTransport is a fake remote that accepts every push, records the
// projects it received, and serves pulls by cursor like the real server.
type routingTransport struct {
	mu      sync.Mutex
	pushed  []string
	nextSeq int64
	feed    []PulledMutation
	since   []int64
}

func (t *routingTransport) PushMutations(entries []MutationEntry) (*PushMutationsResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	seqs := make([]int64, len(entries))
	for i, entry := range entries {
		t.pushed = append(t.pushed, entry.Project)
		t.nextSeq++
		seqs[i] = t.nextSeq
	}
	return &PushMutationsResult{AcceptedSeqs: seqs}, nil
}

func (t *routingTransport) PullMutations(sinceSeq int64, limit int) (*PullMutationsResponse, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.since = append(t.since, sinceSeq)
	out := []PulledMutation{}
	for _, m := range t.feed {
		if m.Seq > sinceSeq && len(out) < limit {
			out = append(out, m)
		}
	}
	return &PullMutationsResponse{Mutations: out}, nil
}

func (t *routingTransport) pushedProjects() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	seen := map[string]struct{}{}
	out := []string{}
	for _, p := range t.pushed {
		if _, ok := seen[p]; !ok {
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func remoteSession(seq int64, project, id string) PulledMutation {
	return PulledMutation{
		Seq:       seq,
		Project:   project,
		Entity:    store.SyncEntitySession,
		EntityKey: id,
		Op:        store.SyncOpUpsert,
		Payload:   json.RawMessage(fmt.Sprintf(`{"id":%q,"project":%q,"directory":"/tmp/%s"}`, id, project, project)),
	}
}

func openRoutingStore(t *testing.T, projects ...string) *store.Store {
	t.Helper()
	cfg, err := store.DefaultConfig()
	if err != nil {
		t.Fatalf("store default config: %v", err)
	}
	cfg.DataDir = t.TempDir()
	local, err := store.New(cfg)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = local.Close() })
	for _, project := range projects {
		if err := local.EnrollProject(project); err != nil {
			t.Fatalf("enroll %s: %v", project, err)
		}
		if err := local.CreateSession("local-"+project, project, "/tmp/"+project); err != nil {
			t.Fatalf("create %s session: %v", project, err)
		}
	}
	return local
}

func sessionExists(t *testing.T, local *store.Store, id string) bool {
	t.Helper()
	var n int
	if err := local.DB().QueryRow(`SELECT COUNT(*) FROM sessions WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count session %s: %v", id, err)
	}
	return n > 0
}

func mustSyncState(t *testing.T, local *store.Store, key string) *store.SyncState {
	t.Helper()
	state, err := local.GetSyncState(key)
	if err != nil {
		t.Fatalf("sync state %s: %v", key, err)
	}
	return state
}

func pendingFor(t *testing.T, local *store.Store, project string) int {
	t.Helper()
	pending, err := local.ListPendingSyncMutations(store.DefaultSyncTargetKey, 1000)
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

func TestManagerStateKeySeparatesJournalFromSyncState(t *testing.T) {
	local := openRoutingStore(t, "alpha")
	tr := &routingTransport{feed: []PulledMutation{remoteSession(7, "alpha", "remote-alpha")}}
	cfg := DefaultConfig()
	cfg.LeaseOwner = "owner-remote"
	cfg.StateKey = "cloud@abc123"
	mgr := New(local, tr, cfg)

	mgr.cycle(context.Background())

	if got := mgr.Status(); got.Phase != PhaseHealthy {
		t.Fatalf("phase = %q (%s), want healthy", got.Phase, got.LastError)
	}
	if n := pendingFor(t, local, "alpha"); n != 0 {
		t.Fatalf("journal rows under %q were not acked: %d pending", store.DefaultSyncTargetKey, n)
	}
	remote := mustSyncState(t, local, "cloud@abc123")
	if remote.LastPulledSeq != 7 {
		t.Fatalf("state-key cursor = %d, want 7", remote.LastPulledSeq)
	}
	if remote.LeaseOwner == nil || *remote.LeaseOwner != "owner-remote" {
		t.Fatalf("state-key lease owner = %v, want owner-remote", remote.LeaseOwner)
	}
	global := mustSyncState(t, local, store.DefaultSyncTargetKey)
	if global.LastPulledSeq != 0 {
		t.Fatalf("journal-key cursor moved to %d", global.LastPulledSeq)
	}
	if global.LeaseOwner != nil && *global.LeaseOwner != "" {
		t.Fatalf("journal-key lease taken by %q", *global.LeaseOwner)
	}
	if !sessionExists(t, local, "remote-alpha") {
		t.Fatal("pulled mutation was not applied")
	}
}

func TestScopedManagersRouteProjectsWithIndependentCursorsAndLeases(t *testing.T) {
	local := openRoutingStore(t, "alpha", "beta", "gamma")

	globalTr := &routingTransport{feed: []PulledMutation{
		remoteSession(1, "alpha", "g-alpha"),
		remoteSession(2, "gamma", "g-gamma"),
		// A routed project's mutation on the global server must not be applied
		// locally, but the cursor still moves past it.
		remoteSession(3, "beta", "g-beta-leak"),
	}}
	remoteTr := &routingTransport{feed: []PulledMutation{
		remoteSession(1, "beta", "r-beta"),
		remoteSession(2, "alpha", "r-alpha-leak"),
	}}

	globalCfg := DefaultConfig()
	globalCfg.LeaseOwner = "owner-global"
	globalCfg.ExcludeProjects = []string{"beta"}
	remoteCfg := DefaultConfig()
	remoteCfg.LeaseOwner = "owner-remote"
	remoteCfg.StateKey = "cloud@r1"
	remoteCfg.IncludeProjects = []string{"beta"}

	globalMgr := New(local, globalTr, globalCfg)
	remoteMgr := New(local, remoteTr, remoteCfg)
	globalMgr.cycle(context.Background())
	remoteMgr.cycle(context.Background())

	for _, mgr := range []*Manager{globalMgr, remoteMgr} {
		if st := mgr.Status(); st.Phase != PhaseHealthy {
			t.Fatalf("phase = %q (%s), want healthy", st.Phase, st.LastError)
		}
	}
	if got := strings.Join(globalTr.pushedProjects(), ","); got != "alpha,gamma" {
		t.Fatalf("global remote received %q, want alpha,gamma", got)
	}
	if got := strings.Join(remoteTr.pushedProjects(), ","); got != "beta" {
		t.Fatalf("project remote received %q, want beta", got)
	}

	if got := mustSyncState(t, local, store.DefaultSyncTargetKey).LastPulledSeq; got != 3 {
		t.Fatalf("global cursor = %d, want 3 (advanced past skipped seq)", got)
	}
	if got := mustSyncState(t, local, "cloud@r1").LastPulledSeq; got != 2 {
		t.Fatalf("remote cursor = %d, want 2 (advanced past skipped seq)", got)
	}
	for id, want := range map[string]bool{"g-alpha": true, "g-gamma": true, "r-beta": true, "g-beta-leak": false, "r-alpha-leak": false} {
		if got := sessionExists(t, local, id); got != want {
			t.Fatalf("session %s applied = %v, want %v", id, got, want)
		}
	}
	for key, owner := range map[string]string{store.DefaultSyncTargetKey: "owner-global", "cloud@r1": "owner-remote"} {
		state := mustSyncState(t, local, key)
		if state.LeaseOwner == nil || *state.LeaseOwner != owner {
			t.Fatalf("lease of %s = %v, want %s", key, state.LeaseOwner, owner)
		}
	}

	// A second cycle resumes each remote from its own cursor.
	globalMgr.cycle(context.Background())
	remoteMgr.cycle(context.Background())
	if last := globalTr.since[len(globalTr.since)-1]; last != 3 {
		t.Fatalf("global resumed from %d, want 3", last)
	}
	if last := remoteTr.since[len(remoteTr.since)-1]; last != 2 {
		t.Fatalf("remote resumed from %d, want 2", last)
	}
}

func TestScopedManagerIsNotStarvedByOtherRemotesRows(t *testing.T) {
	local := openRoutingStore(t, "alpha")
	// 150 alpha rows sit ahead of the only beta row.
	for i := 0; i < 150; i++ {
		id := fmt.Sprintf("alpha-bulk-%03d", i)
		if _, err := local.DB().Exec(`
			INSERT INTO sync_mutations (target_key, entity, entity_key, op, payload, source, project)
			VALUES (?, 'session', ?, 'upsert', ?, 'local', 'alpha')`,
			store.DefaultSyncTargetKey, id, fmt.Sprintf(`{"id":%q,"directory":"/tmp/alpha","project":"alpha"}`, id)); err != nil {
			t.Fatalf("seed alpha row: %v", err)
		}
	}
	if err := local.EnrollProject("beta"); err != nil {
		t.Fatalf("enroll beta: %v", err)
	}
	if err := local.CreateSession("local-beta", "beta", "/tmp/beta"); err != nil {
		t.Fatalf("create beta session: %v", err)
	}

	tr := &routingTransport{}
	cfg := DefaultConfig()
	cfg.StateKey = "cloud@r1"
	cfg.IncludeProjects = []string{"beta"}
	cfg.PushBatchSize = 100
	if err := New(local, tr, cfg).push(context.Background()); err != nil {
		t.Fatalf("push: %v", err)
	}
	if got := strings.Join(tr.pushedProjects(), ","); got != "beta" {
		t.Fatalf("scoped push sent %q, want beta", got)
	}
	if pendingFor(t, local, "beta") != 0 {
		t.Fatal("beta row still pending")
	}
	if pendingFor(t, local, "alpha") == 0 {
		t.Fatal("alpha rows of another remote were acked by the scoped manager")
	}
}

func TestScopedManagerIgnoresNonEnrolledProjectsOutsideItsScope(t *testing.T) {
	local := openRoutingStore(t)
	if _, err := local.DB().Exec(`
		INSERT INTO sync_mutations (target_key, entity, entity_key, op, payload, source, project)
		VALUES (?, 'session', 'orphan', 'upsert', '{"id":"orphan","directory":"/tmp/o","project":"other"}', 'local', 'other')`,
		store.DefaultSyncTargetKey); err != nil {
		t.Fatalf("seed non-enrolled row: %v", err)
	}
	cfg := DefaultConfig()
	cfg.StateKey = "cloud@r1"
	cfg.IncludeProjects = []string{"beta"}
	if err := New(local, &routingTransport{}, cfg).push(context.Background()); err != nil {
		t.Fatalf("scoped manager reported another remote's non-enrolled backlog: %v", err)
	}
}
