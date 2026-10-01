package store

import (
	"strings"
	"testing"
)

func countPendingProjectMutations(t *testing.T, s *Store, project string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sync_mutations WHERE project = ? AND acked_at IS NULL AND disposition = 'pending'`, project).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func seedAckedProject(t *testing.T, s *Store, session, project string) {
	t.Helper()
	if err := s.CreateSession(session, project, "/tmp/"+project); err != nil {
		t.Fatal(err)
	}
	if err := s.EnrollProject(project); err != nil {
		t.Fatal(err)
	}
	addTestObsSession(t, s, session, "first "+project, "decision", project, "project")
	addTestObsSession(t, s, session, "second "+project, "decision", project, "project")
	if _, err := s.AddPrompt(AddPromptParams{SessionID: session, Content: "prompt " + project, Project: project}); err != nil {
		t.Fatal(err)
	}
}

func ackAllMutations(t *testing.T, s *Store) {
	t.Helper()
	var maxSeq int64
	if err := s.db.QueryRow(`SELECT MAX(seq) FROM sync_mutations`).Scan(&maxSeq); err != nil {
		t.Fatal(err)
	}
	if err := s.AckSyncMutations(DefaultSyncTargetKey, maxSeq); err != nil {
		t.Fatal(err)
	}
}

func TestRequeueProjectSyncHistoryReplaysOnlyThatProject(t *testing.T) {
	s := newTestStore(t)
	seedAckedProject(t, s, "moved-session", "moved")
	seedAckedProject(t, s, "stays-session", "stays")
	ackAllMutations(t, s)
	if got := countPendingProjectMutations(t, s, "moved"); got != 0 {
		t.Fatalf("precondition: moved pending = %d, want 0", got)
	}
	var ackedBefore int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sync_mutations WHERE acked_at IS NOT NULL`).Scan(&ackedBefore); err != nil {
		t.Fatal(err)
	}

	queued, err := s.RequeueProjectSyncHistory(" moved ")
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	// session + 2 observations + 1 prompt
	if queued != 4 {
		t.Fatalf("queued = %d, want 4", queued)
	}
	if got := countPendingProjectMutations(t, s, "moved"); got != 4 {
		t.Fatalf("moved pending = %d, want 4", got)
	}
	if got := countPendingProjectMutations(t, s, "stays"); got != 0 {
		t.Fatalf("stays pending = %d, want 0 (other projects untouched)", got)
	}
	var ackedAfter int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sync_mutations WHERE acked_at IS NOT NULL`).Scan(&ackedAfter); err != nil {
		t.Fatal(err)
	}
	if ackedAfter != ackedBefore {
		t.Fatalf("acked history rewritten: before %d after %d", ackedBefore, ackedAfter)
	}
	pending, err := s.ListPendingSyncMutations(DefaultSyncTargetKey, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 4 {
		t.Fatalf("pending deliverable mutations = %d, want 4", len(pending))
	}
	for _, m := range pending {
		if m.Project != "moved" {
			t.Fatalf("pending mutation for unexpected project %q", m.Project)
		}
	}
}

func TestRequeueProjectSyncHistoryIsIdempotentWhilePending(t *testing.T) {
	s := newTestStore(t)
	seedAckedProject(t, s, "idem-session", "idem")
	ackAllMutations(t, s)

	first, err := s.RequeueProjectSyncHistory("idem")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.RequeueProjectSyncHistory("idem")
	if err != nil {
		t.Fatal(err)
	}
	if first == 0 || second != 0 {
		t.Fatalf("queued first=%d second=%d, want >0 then 0", first, second)
	}
	if got := countPendingProjectMutations(t, s, "idem"); got != first {
		t.Fatalf("pending = %d, want %d", got, first)
	}

	// Once delivered, a later reassignment replays the history again.
	ackAllMutations(t, s)
	third, err := s.RequeueProjectSyncHistory("idem")
	if err != nil {
		t.Fatal(err)
	}
	if third != first {
		t.Fatalf("queued after ack = %d, want %d", third, first)
	}
}

func TestRequeueProjectSyncHistorySkipsUnenrolledProject(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("loose-session", "loose", "/tmp/loose"); err != nil {
		t.Fatal(err)
	}
	queued, err := s.RequeueProjectSyncHistory("loose")
	if err != ErrProjectNotEnrolled {
		t.Fatalf("err = %v, want ErrProjectNotEnrolled", err)
	}
	if queued != 0 {
		t.Fatalf("queued = %d, want 0", queued)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sync_mutations WHERE project = 'loose'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("unenrolled project gained %d mutation rows", n)
	}
	if _, err := s.RequeueProjectSyncHistory("   "); err == nil {
		t.Fatalf("expected error for empty project")
	}
}

func TestRequeueProjectSyncHistoryReplaysAgainAfterPartialDelivery(t *testing.T) {
	s := newTestStore(t)
	seedAckedProject(t, s, "partial-session", "partial")
	ackAllMutations(t, s)
	first, err := s.RequeueProjectSyncHistory("partial")
	if err != nil {
		t.Fatal(err)
	}
	var firstSeq int64
	if err := s.db.QueryRow(`SELECT MIN(seq) FROM sync_mutations WHERE project = 'partial' AND acked_at IS NULL`).Scan(&firstSeq); err != nil {
		t.Fatal(err)
	}
	// The previous remote acknowledged only part of the replay.
	if err := s.AckSyncMutationSeqs(DefaultSyncTargetKey, []int64{firstSeq}); err != nil {
		t.Fatal(err)
	}
	second, err := s.RequeueProjectSyncHistory("partial")
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("queued after partial delivery = %d, want full replay %d", second, first)
	}
}

func seedPendingSessionRow(t *testing.T, s *Store, project, id string) int64 {
	t.Helper()
	res, err := s.db.Exec(`
		INSERT INTO sync_mutations (target_key, entity, entity_key, op, payload, source, project)
		VALUES (?, 'session', ?, 'upsert', ?, 'local', ?)`,
		DefaultSyncTargetKey, id, `{"id":"`+id+`","directory":"/tmp/x","project":"`+project+`"}`, project)
	if err != nil {
		t.Fatal(err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

func pendingProjects(rows []SyncMutation) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Project)
	}
	return out
}

func TestListPendingSyncMutationsScopedSelectsInTheQuery(t *testing.T) {
	s := newTestStore(t)
	for _, project := range []string{"alpha", "beta", "gamma"} {
		if err := s.EnrollProject(project); err != nil {
			t.Fatal(err)
		}
	}
	// Five alpha rows at the head of the journal would fill a LIMIT 2 batch
	// if scope were applied after the query.
	for i := 0; i < 5; i++ {
		seedPendingSessionRow(t, s, "alpha", "alpha-"+string(rune('a'+i)))
	}
	betaSeq := seedPendingSessionRow(t, s, "beta", "beta-1")
	seedPendingSessionRow(t, s, "gamma", "gamma-1")
	seedPendingSessionRow(t, s, "", "global-1")
	seedPendingSessionRow(t, s, "unenrolled", "unenrolled-1")

	included, err := s.ListPendingSyncMutationsScoped(DefaultSyncTargetKey, []string{"beta"}, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(included) != 1 || included[0].Seq != betaSeq {
		t.Fatalf("include beta = %v, want only seq %d", pendingProjects(included), betaSeq)
	}

	excluded, err := s.ListPendingSyncMutationsScoped(DefaultSyncTargetKey, nil, []string{"alpha"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(pendingProjects(excluded), ","); got != "beta,gamma," {
		t.Fatalf("exclude alpha = %q, want beta,gamma and the empty-project row", got)
	}

	none, err := s.ListPendingSyncMutationsScoped(DefaultSyncTargetKey, []string{}, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("empty include set must select nothing, got %v", pendingProjects(none))
	}

	legacy, err := s.ListPendingSyncMutations(DefaultSyncTargetKey, 100)
	if err != nil {
		t.Fatal(err)
	}
	unscoped, err := s.ListPendingSyncMutationsScoped(DefaultSyncTargetKey, nil, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(pendingProjects(unscoped), ",") != strings.Join(pendingProjects(legacy), ",") {
		t.Fatalf("nil scope = %v, want legacy %v", pendingProjects(unscoped), pendingProjects(legacy))
	}
}

func TestAdvanceSyncPullCursorOnlyMovesForward(t *testing.T) {
	s := newTestStore(t)
	const key = "cloud@abc123"
	if err := s.AdvanceSyncPullCursor(key, 7); err != nil {
		t.Fatal(err)
	}
	if err := s.AdvanceSyncPullCursor(key, 3); err != nil {
		t.Fatal(err)
	}
	state, err := s.GetSyncState(key)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastPulledSeq != 7 {
		t.Fatalf("cursor = %d, want 7", state.LastPulledSeq)
	}
	global, err := s.GetSyncState(DefaultSyncTargetKey)
	if err != nil {
		t.Fatal(err)
	}
	if global.LastPulledSeq != 0 {
		t.Fatalf("global cursor moved to %d", global.LastPulledSeq)
	}
}


func seedSyncStateRow(t *testing.T, s *Store, targetKey string) {
	t.Helper()
	if _, err := s.DB().Exec(`INSERT INTO sync_state (target_key, lifecycle, last_pulled_seq, updated_at) VALUES (?, 'idle', 42, datetime('now'))`, targetKey); err != nil {
		t.Fatalf("seed sync_state %q: %v", targetKey, err)
	}
}

func syncStateExists(t *testing.T, s *Store, targetKey string) bool {
	t.Helper()
	return scalarInt(t, s, `SELECT COUNT(*) FROM sync_state WHERE target_key = ?`, targetKey) == 1
}

func cleanupActionFor(report ForeignSyncTargetCleanupReport, targetKey string) (ForeignSyncTargetCleanupAction, bool) {
	for _, action := range report.Actions {
		if action.TargetKey == targetKey {
			return action, true
		}
	}
	return ForeignSyncTargetCleanupAction{}, false
}

func TestCleanupForeignSyncTargetsKeepsLiveRemoteStateAndPrunesOrphans(t *testing.T) {
	s := newTestStore(t)
	const live, orphan, foreign = "cloud@aaaaaaaaaaaa", "cloud@bbbbbbbbbbbb", "satellite:inert"
	for _, key := range []string{live, orphan, foreign} {
		seedSyncStateRow(t, s, key)
	}

	planned, err := s.CleanupForeignSyncTargetsWithLiveRemotes(false, map[string]bool{live: true})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, listed := cleanupActionFor(planned, live); listed {
		t.Fatalf("live remote state must never be a cleanup action: %+v", planned)
	}
	orphanAction, listed := cleanupActionFor(planned, orphan)
	if !listed || !orphanAction.StateRemoved || orphanAction.Reason != ForeignSyncTargetReasonOrphanedRemoteState {
		t.Fatalf("orphan action = %+v (listed=%v), want a pruned orphaned remote state", orphanAction, listed)
	}
	if foreignAction, ok := cleanupActionFor(planned, foreign); !ok || foreignAction.Reason != "" || !foreignAction.StateRemoved {
		t.Fatalf("legacy foreign action changed: %+v", planned)
	}
	if !syncStateExists(t, s, orphan) {
		t.Fatal("planning must not delete state")
	}

	applied, err := s.CleanupForeignSyncTargetsWithLiveRemotes(true, map[string]bool{live: true})
	if err != nil || !applied.Applied {
		t.Fatalf("apply: %+v, %v", applied, err)
	}
	if !syncStateExists(t, s, live) {
		t.Fatal("live cloud@ state was deleted")
	}
	if syncStateExists(t, s, orphan) || syncStateExists(t, s, foreign) {
		t.Fatal("orphaned cloud@ state and legacy foreign state must be pruned")
	}
}

func TestCleanupForeignSyncTargetsUnknownRemotesPrunesNoRemoteState(t *testing.T) {
	s := newTestStore(t)
	const remoteKey, foreign = "cloud@cccccccccccc", "satellite:inert"
	seedSyncStateRow(t, s, remoteKey)
	seedSyncStateRow(t, s, foreign)

	// nil means the configured remotes are unknown (cloud.json unreadable, or
	// the legacy entry point): every cloud@ row is kept.
	report, err := s.CleanupForeignSyncTargetsWithLiveRemotes(true, nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, listed := cleanupActionFor(report, remoteKey); listed || !syncStateExists(t, s, remoteKey) {
		t.Fatalf("unknown remotes must prune nothing under cloud@: %+v", report)
	}
	if syncStateExists(t, s, foreign) {
		t.Fatal("legacy foreign cleanup must still run")
	}

	seedSyncStateRow(t, s, "satellite:again")
	legacy, err := s.CleanupForeignSyncTargets(true)
	if err != nil {
		t.Fatalf("legacy cleanup: %v", err)
	}
	if _, listed := cleanupActionFor(legacy, remoteKey); listed || !syncStateExists(t, s, remoteKey) {
		t.Fatalf("legacy entry point must never touch cloud@ state: %+v", legacy)
	}
}

func TestCleanupForeignSyncTargetsRetainsOrphanWithDeferredRows(t *testing.T) {
	s := newTestStore(t)
	const orphan = "cloud@dddddddddddd"
	seedSyncStateRow(t, s, orphan)
	if _, err := s.DB().Exec(`INSERT INTO sync_apply_deferred (sync_id, entity, payload, target_key) VALUES ('deferred-1', 'observation', '{}', ?)`, orphan); err != nil {
		t.Fatalf("seed deferred row: %v", err)
	}

	report, err := s.CleanupForeignSyncTargetsWithLiveRemotes(true, map[string]bool{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	action, listed := cleanupActionFor(report, orphan)
	if !listed || action.StateRemoved || action.RetainedMutations != 1 {
		t.Fatalf("orphan with deferred rows = %+v (listed=%v), want retained", action, listed)
	}
	if !syncStateExists(t, s, orphan) {
		t.Fatal("orphan state with deferred rows must be kept")
	}
}
