package store

import "testing"

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
