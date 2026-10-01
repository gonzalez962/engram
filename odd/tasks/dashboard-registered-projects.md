# Feature: dashboard lists admin-registered projects

Locator: `odd/tasks/dashboard-registered-projects.md` · Engram mirror: `odd/dashboard-registered-projects/tasks`
Branch: `fix/dashboard-registered-projects` (from `origin/main` @ 7182fa4)

## Objective
Managed principals (bootstrap admin and other managed users) must see, in the cloud dashboard,
the projects they hold grants for when those projects were registered by an admin, even when the
project is not in `ENGRAM_CLOUD_ALLOWED_PROJECTS` and even before any chunk has been synced.

## Problem / why
- `DashboardStoreForProjects` intersects principal grants with the startup-only env allowlist
  (`internal/cloud/cloudstore/dashboard_queries.go:1109-1131`); the base read model is filtered by
  the same allowlist (`dashboard_queries.go:~940`). Projects created via `POST /admin/projects`
  (`admin_handlers.go:413`) never enter that allowlist, so the list is empty for every managed user.
- Push for managed principals is already authorized by grants only (`cloudserver.go:855-860`),
  so the dashboard is inconsistent with the sync path.
- The read model builds project rows from chunk rows only (`dashboard_queries.go:152-157`), so a
  registered project with zero chunks never appears.

## Constraints (user decision)
- The cloud must NOT accept/show arbitrary projects: no automatic `*`. Visible deployment scope =
  env allowlist ∪ projects registered by an admin (`cloud_project_controls`).
- Per-principal grants still restrict each user's view.
- Legacy (non-managed) dashboard sessions keep current behavior.

## Tasks
- [x] T1 — Dashboard deployment scope = env allowlist ∪ registered projects (`cloud_project_controls`),
  applied to the base read-model filter and to `DashboardStoreForProjects`; invalidate the read
  model cache when a project is created. RED test: managed admin creates project not in allowlist,
  has synced chunks, and sees it listed.
- [x] T2 — Registered projects with zero chunks appear in the project list (zero counts) within
  scope. RED test: freshly created project appears in `ListProjects` for its grantee and not for
  a principal without grant.

## Route
Delegated direct: one writer (2+ non-trivial files in `internal/cloud/cloudstore`, tests).
Trigger evidence: mapping needed >5 lookups (explorer used); write touches multiple non-trivial files.

## Acceptance criteria
- Projects in `cloud_project_controls` with a grant appear for the grantee in `/dashboard/projects/list`.
- Projects neither in the env allowlist nor registered stay hidden.
- Existing dashboard principal / allowlist tests stay green.

## Checks
- `go test ./internal/cloud/...`
- `go vet ./internal/cloud/...`

## Progress
- Created feature doc; branch created.
- T1 done — commit `87fcdea` `fix(cloud): include registered projects in dashboard scope`. Route: delegated
  direct (one writer). New `internal/cloud/cloudstore/dashboard_scope.go`: `effectiveDashboardScope`
  (env allowlist ∪ `cloud_project_controls`; `*` or empty allowlist keeps unrestricted behavior; registered
  names never add a wildcard), `CloudStore.dashboardScope`, `loadRegisteredDashboardProjects`. The read model
  carries `registeredProjects`; `buildDashboardReadModel`, `loadChunkRows`/`loadMutationRows`,
  `DashboardStoreForProjects` and `normalizeDashboardProject` use the single scope helper.
  `normalizeDashboardProject` fails closed (`ErrDashboardProjectForbidden` wrapping the cause) when the
  registry cannot be loaded. `CreateProjectWithGrantAndAudit` invalidates the read-model cache after commit.
  RED observed: `TestDashboardScopeIncludesAdminRegisteredProjects` (Postgres) failed with
  "expected deployment scope to list env and registered projects, got [env-project]"; GREEN after fix.
- T2 done — commit `dc913fb` `fix(cloud): list registered projects without synced chunks`. Route: delegated
  direct. `dashboardReadModel.withRegisteredProjects` adds zero-count rows + empty details before scoping.
  RED observed: `TestDashboardListsRegisteredProjectsWithoutChunks` (Postgres) failed with
  "expected registered project without chunks in deployment list, got [env-project]"; GREEN after fix.
- Pure unit tests (run without Postgres): `TestEffectiveDashboardScopeUnionsAllowlistAndRegisteredProjects`,
  `TestDashboardStoreForProjectsHonorsRegisteredProjectsInDeploymentScope`,
  `TestNormalizeDashboardProjectFailsClosedWhenRegistryIsUnavailable`,
  `TestReadModelWithRegisteredProjectsAddsZeroCountRows` (written after the DB RED, not RED-first).

## Verification evidence (local, Windows, Postgres 16 container via `CLOUDSTORE_TEST_DSN`)
- `go build ./...`: ok
- `go vet ./internal/cloud/... ./cmd/engram/...`: ok
- `go test ./internal/cloud/...`: all ok except `chunkcodec` `TestCompressedEnvelopeRoundTripDoesNotExposePayloadText`,
  which also fails on base `7182fa4` (pre-existing, untouched package). Same result with and without DSN.
- `go test ./cmd/engram/ -run 'Cloud|Dashboard' -count=1`: ok (with and without DSN)

## Review
- RDD assess (base 7182fa4, committed-only): medium, `review_due=slice_budget_reached`. Consent: granted.
- Lineage `review-3ddde1aee3e163d0`, one lens (reliability): approved and acknowledged.
- Advisory (non-blocking) follow-ups:
  - R3-cache-invalidation-scope: other writes to `cloud_project_controls` (pause/resume, removal)
    must also invalidate the dashboard read model; consider a generation guard against stale builds.
  - R3-failclosed-masks-outage: registry load failure maps to Forbidden (403) instead of 5xx.

## Next step
User decides on push/PR. Pre-existing chunkcodec failure is out of scope.
