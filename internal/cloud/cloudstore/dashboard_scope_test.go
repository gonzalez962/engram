package cloudstore

import (
	"context"
	"errors"
	"testing"

	"github.com/Gentleman-Programming/engram/v2/internal/cloud/chunkcodec"
)

// writeDashboardScopeChunk persists one canonical chunk with a single session
// for project so the dashboard read model has a row to materialize.
func writeDashboardScopeChunk(t *testing.T, cs *CloudStore, project string) {
	t.Helper()
	payload, err := chunkcodec.CanonicalizeForProject([]byte(`{
		"sessions":[{"id":"s-`+project+`","directory":"/tmp/`+project+`","started_at":"2026-04-29T10:00:00Z"}]
	}`), project)
	if err != nil {
		t.Fatalf("canonicalize chunk for %s: %v", project, err)
	}
	if err := cs.WriteChunk(context.Background(), project, chunkIDFromPayload(payload), "tester", "2026-04-29T10:03:00Z", payload); err != nil {
		t.Fatalf("WriteChunk %s: %v", project, err)
	}
}

func dashboardProjectNames(rows []DashboardProjectRow) []string {
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Project)
	}
	return names
}

func containsDashboardProject(rows []DashboardProjectRow, project string) bool {
	for _, row := range rows {
		if row.Project == project {
			return true
		}
	}
	return false
}

// TestDashboardScopeIncludesAdminRegisteredProjects proves the deployment
// scope is the env allowlist plus projects registered in
// cloud_project_controls: a project created through
// CreateProjectWithGrantAndAudit becomes visible immediately (cache
// invalidated) to its grantee, while a project that is neither allowlisted nor
// registered stays hidden even when a principal holds a grant for it.
func TestDashboardScopeIncludesAdminRegisteredProjects(t *testing.T) {
	ctx := context.Background()
	cs := openIsolatedCloudStore(t)
	cs.SetDashboardAllowedProjects([]string{"env-project"})

	admin, err := cs.CreateHumanUser(ctx, CreateHumanUserParams{Username: "scope-admin", Email: "scope-admin@example.test", DisplayName: "Scope Admin", Role: PrincipalRoleAdmin})
	if err != nil {
		t.Fatalf("CreateHumanUser: %v", err)
	}

	writeDashboardScopeChunk(t, cs, "env-project")
	writeDashboardScopeChunk(t, cs, "registered-project")
	writeDashboardScopeChunk(t, cs, "rogue-project")

	// Prime the cache before registration so the assertions below prove that
	// project creation invalidates the cached read model.
	before, err := cs.ListProjects("")
	if err != nil {
		t.Fatalf("ListProjects before registration: %v", err)
	}
	if containsDashboardProject(before, "registered-project") {
		t.Fatalf("expected unregistered project to be hidden before registration, got %v", dashboardProjectNames(before))
	}

	if err := cs.CreateProjectWithGrantAndAudit(ctx,
		CreateProjectGrantParams{PrincipalID: admin.PrincipalID, Project: "registered-project", GrantedByPrincipalID: admin.PrincipalID},
		AuthAuditEvent{ActorPrincipalID: admin.PrincipalID, ActorSource: "managed", Project: "registered-project", Action: "project.create", Outcome: "success"},
	); err != nil {
		t.Fatalf("CreateProjectWithGrantAndAudit: %v", err)
	}

	after, err := cs.ListProjects("")
	if err != nil {
		t.Fatalf("ListProjects after registration: %v", err)
	}
	if !containsDashboardProject(after, "env-project") || !containsDashboardProject(after, "registered-project") {
		t.Fatalf("expected deployment scope to list env and registered projects, got %v", dashboardProjectNames(after))
	}
	if containsDashboardProject(after, "rogue-project") {
		t.Fatalf("expected project outside allowlist and registry to stay hidden, got %v", dashboardProjectNames(after))
	}
	if _, err := cs.ProjectDetail("registered-project"); err != nil {
		t.Fatalf("expected registered project detail to be readable, got %v", err)
	}
	if _, err := cs.ProjectDetail("rogue-project"); !errors.Is(err, ErrDashboardProjectForbidden) {
		t.Fatalf("expected rogue project detail to be forbidden, got %v", err)
	}

	grants, err := cs.ListProjectGrants(ctx, admin.PrincipalID)
	if err != nil {
		t.Fatalf("ListProjectGrants: %v", err)
	}
	granted := make([]string, 0, len(grants))
	for _, grant := range grants {
		granted = append(granted, grant.Project)
	}
	view, err := cs.DashboardStoreForProjects(granted)
	if err != nil {
		t.Fatalf("DashboardStoreForProjects: %v", err)
	}
	rows, err := view.ListProjects("")
	if err != nil {
		t.Fatalf("scoped ListProjects: %v", err)
	}
	if len(rows) != 1 || rows[0].Project != "registered-project" {
		t.Fatalf("expected grantee to see exactly the registered project, got %v", dashboardProjectNames(rows))
	}

	rogueView, err := cs.DashboardStoreForProjects([]string{"rogue-project"})
	if err != nil {
		t.Fatalf("DashboardStoreForProjects rogue: %v", err)
	}
	if rows, err := rogueView.ListProjects(""); err != nil || len(rows) != 0 {
		t.Fatalf("expected grant outside deployment scope to expose nothing, rows=%v err=%v", dashboardProjectNames(rows), err)
	}
}

func TestEffectiveDashboardScopeUnionsAllowlistAndRegisteredProjects(t *testing.T) {
	env := map[string]struct{}{"env-project": {}}

	scope := effectiveDashboardScope(false, env, []string{" registered-project ", "", "*"})
	if scope.all {
		t.Fatalf("expected registered projects never to widen scope to a wildcard")
	}
	for _, project := range []string{"env-project", "registered-project"} {
		if !scope.allows(project) {
			t.Fatalf("expected %s inside effective scope, got %v", project, scope.sortedProjects())
		}
	}
	if scope.allows("rogue-project") || scope.allows("*") {
		t.Fatalf("expected projects outside allowlist and registry to stay out of scope, got %v", scope.sortedProjects())
	}
	if got := scope.sortedProjects(); len(got) != 2 || got[0] != "env-project" || got[1] != "registered-project" {
		t.Fatalf("expected sorted union [env-project registered-project], got %v", got)
	}

	if !effectiveDashboardScope(true, nil, []string{"registered-project"}).all {
		t.Fatalf("expected wildcard allowlist to keep unrestricted scope")
	}
	if !effectiveDashboardScope(false, nil, []string{"registered-project"}).all {
		t.Fatalf("expected empty allowlist to keep the historical unrestricted scope")
	}
	if empty := (dashboardReadModel{projects: []DashboardProjectRow{{Project: "x"}}}).scopedTo(dashboardDeploymentScope{}); len(empty.projects) != 0 {
		t.Fatalf("expected restricted scope without projects to yield an empty model, got %+v", empty.projects)
	}
}

func TestDashboardStoreForProjectsHonorsRegisteredProjectsInDeploymentScope(t *testing.T) {
	model := dashboardPrincipalReadModel()
	model.registeredProjects = []string{"project-b"}
	store := &CloudStore{
		dashboardAllowedScopes: map[string]struct{}{"project-a": {}},
		dashboardReadModel:     model,
		dashboardReadModelOK:   true,
	}

	granted, err := store.DashboardStoreForProjects([]string{"project-b", "outside-deployment"})
	if err != nil {
		t.Fatalf("DashboardStoreForProjects: %v", err)
	}
	assertDashboardPrincipalProjects(t, granted, "project-b")
	if _, err := granted.ProjectDetail("project-b"); err != nil {
		t.Fatalf("expected registered granted project detail, got %v", err)
	}
	if _, err := granted.ProjectDetail("project-a"); !errors.Is(err, ErrDashboardProjectForbidden) {
		t.Fatalf("expected ungranted allowlisted project to stay forbidden, got %v", err)
	}

	wildcard, err := store.DashboardStoreForProjects([]string{"*"})
	if err != nil {
		t.Fatalf("DashboardStoreForProjects wildcard: %v", err)
	}
	if rows, err := wildcard.ListProjects(""); err != nil || len(rows) != 2 || rows[0].Project != "project-a" || rows[1].Project != "project-b" {
		t.Fatalf("expected wildcard principal to see allowlisted and registered projects, rows=%v err=%v", dashboardProjectNames(rows), err)
	}
	if _, err := wildcard.scopedProject("outside-deployment"); !errors.Is(err, ErrDashboardProjectForbidden) {
		t.Fatalf("expected wildcard principal to stay inside deployment scope, got %v", err)
	}

	if _, err := store.normalizeDashboardProject("project-b"); err != nil {
		t.Fatalf("expected registered project to pass deployment normalization, got %v", err)
	}
	if _, err := store.normalizeDashboardProject("outside-deployment"); !errors.Is(err, ErrDashboardProjectForbidden) {
		t.Fatalf("expected unregistered project to be forbidden, got %v", err)
	}
}

func TestNormalizeDashboardProjectFailsClosedWhenRegistryIsUnavailable(t *testing.T) {
	store := &CloudStore{
		dashboardAllowedScopes: map[string]struct{}{"project-a": {}},
		dashboardReadModelLoad: func() (dashboardReadModel, error) { return dashboardReadModel{}, errors.New("db down") },
	}
	if _, err := store.normalizeDashboardProject("project-a"); err != nil {
		t.Fatalf("expected allowlisted project to resolve without the registry, got %v", err)
	}
	_, err := store.normalizeDashboardProject("project-b")
	if !errors.Is(err, ErrDashboardProjectForbidden) {
		t.Fatalf("expected registry failure to fail closed as forbidden, got %v", err)
	}
}

// TestDashboardListsRegisteredProjectsWithoutChunks proves a freshly
// registered project with no synced chunks appears as a zero-count row for its
// grantee, its detail resolves instead of 404, and it stays hidden from a
// principal without a grant.
func TestDashboardListsRegisteredProjectsWithoutChunks(t *testing.T) {
	ctx := context.Background()
	cs := openIsolatedCloudStore(t)
	cs.SetDashboardAllowedProjects([]string{"env-project"})

	admin, err := cs.CreateHumanUser(ctx, CreateHumanUserParams{Username: "empty-admin", Email: "empty-admin@example.test", DisplayName: "Empty Admin", Role: PrincipalRoleAdmin})
	if err != nil {
		t.Fatalf("CreateHumanUser: %v", err)
	}
	writeDashboardScopeChunk(t, cs, "env-project")
	if err := cs.CreateProjectWithGrantAndAudit(ctx,
		CreateProjectGrantParams{PrincipalID: admin.PrincipalID, Project: "Fresh Project", GrantedByPrincipalID: admin.PrincipalID},
		AuthAuditEvent{ActorPrincipalID: admin.PrincipalID, ActorSource: "managed", Project: "Fresh Project", Action: "project.create", Outcome: "success"},
	); err != nil {
		t.Fatalf("CreateProjectWithGrantAndAudit: %v", err)
	}

	all, err := cs.ListProjects("")
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if !containsDashboardProject(all, "fresh-project") {
		t.Fatalf("expected registered project without chunks in deployment list, got %v", dashboardProjectNames(all))
	}
	overview, err := cs.AdminOverview()
	if err != nil {
		t.Fatalf("AdminOverview: %v", err)
	}
	if overview.Projects != 2 || overview.Chunks != 1 {
		t.Fatalf("expected overview to count the empty project with zero chunks, got %+v", overview)
	}

	view, err := cs.DashboardStoreForProjects([]string{"Fresh Project"})
	if err != nil {
		t.Fatalf("DashboardStoreForProjects grantee: %v", err)
	}
	rows, err := view.ListProjects("")
	if err != nil {
		t.Fatalf("grantee ListProjects: %v", err)
	}
	want := DashboardProjectRow{Project: "fresh-project"}
	if len(rows) != 1 || rows[0] != want {
		t.Fatalf("expected grantee to see one zero-count row %+v, got %+v", want, rows)
	}
	detail, err := view.ProjectDetail("fresh-project")
	if err != nil {
		t.Fatalf("expected empty registered project detail, got %v", err)
	}
	if detail.Project != "fresh-project" || detail.Stats != want || len(detail.Sessions) != 0 {
		t.Fatalf("expected empty detail for fresh-project, got %+v", detail)
	}
	if sessions, err := view.ListRecentSessions("fresh-project", "", 10); err != nil || len(sessions) != 0 {
		t.Fatalf("expected empty sessions for fresh-project, rows=%+v err=%v", sessions, err)
	}

	other, err := cs.DashboardStoreForProjects([]string{"env-project"})
	if err != nil {
		t.Fatalf("DashboardStoreForProjects other: %v", err)
	}
	otherRows, err := other.ListProjects("")
	if err != nil {
		t.Fatalf("other ListProjects: %v", err)
	}
	if containsDashboardProject(otherRows, "fresh-project") {
		t.Fatalf("expected principal without grant not to see fresh-project, got %v", dashboardProjectNames(otherRows))
	}
	if _, err := other.ProjectDetail("fresh-project"); !errors.Is(err, ErrDashboardProjectForbidden) {
		t.Fatalf("expected ungranted empty project detail to be forbidden, got %v", err)
	}
}

func TestReadModelWithRegisteredProjectsAddsZeroCountRows(t *testing.T) {
	base := dashboardPrincipalReadModel()
	model := base.withRegisteredProjects([]string{"project-a", " fresh-project ", ""})

	if got := dashboardProjectNames(model.projects); len(got) != 3 || got[0] != "fresh-project" || got[1] != "project-a" || got[2] != "project-b" {
		t.Fatalf("expected sorted rows including the empty registered project, got %v", got)
	}
	if model.admin.Projects != 3 || model.admin.Chunks != base.admin.Chunks {
		t.Fatalf("expected overview to count the empty project without adding chunks, got %+v", model.admin)
	}
	detail, ok := model.projectDetails["fresh-project"]
	if !ok || detail.Stats != (DashboardProjectRow{Project: "fresh-project"}) || len(detail.Sessions) != 0 {
		t.Fatalf("expected empty detail for fresh-project, got %+v (ok=%v)", detail, ok)
	}
	if model.projectDetails["project-a"].Stats != base.projectDetails["project-a"].Stats {
		t.Fatalf("expected existing project stats to stay untouched")
	}
	if _, leaked := base.projectDetails["fresh-project"]; leaked || len(base.projects) != 2 {
		t.Fatalf("expected the source model not to be mutated")
	}

	scoped := model.scopedTo(effectiveDashboardScope(false, map[string]struct{}{"project-a": {}}, nil))
	if got := dashboardProjectNames(scoped.projects); len(got) != 1 || got[0] != "project-a" {
		t.Fatalf("expected deployment scope to drop unscoped empty projects, got %v", got)
	}
}
