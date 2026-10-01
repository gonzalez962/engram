# Feature: per-project cloud remote

Locator: `odd/tasks/per-project-cloud-remote.md` · Engram mirror: `odd/per-project-cloud-remote/tasks`
Branch: `feat/per-project-cloud-remote` (from `feat/cloud-project-api` @ 6961d7e)

## Objective
Keep one global cloud server URL + token as the default, and allow any single project to be
assigned its own server URL + token so that project syncs to a different Engram Cloud.

## Problem / why
The client assumes exactly one remote: one `cloud.json`, one journal (`target_key='cloud'`),
one pull cursor (`sync_state['cloud'].last_pulled_seq`, in the server's seq space), one
autosync manager per process. Users need to separate projects by environment/cloud.

## Design (accepted)
- `cloud.json` gains optional `projects: {"<project>": {server_url, token}}`. Unlisted projects
  use the global config. Env overrides (`ENGRAM_CLOUD_SERVER`, `ENGRAM_CLOUD_TOKEN`) apply to the
  global remote only.
- Journal stays `target_key='cloud'`. Each project has exactly one destination, so the global
  `acked_at` stays correct.
- Autosync runs one manager per distinct remote. Non-global remotes use their own sync_state key
  `cloud@<remote-id>` for pull cursor + lease; push lists only the manager's projects; pull
  applies only the manager's projects. The global manager keeps `cloud` and excludes routed
  projects.
- Rejected: one journal per remote (touches enqueue/ack/backfills/doctor ~20 hardcoded sites,
  no benefit while each project has one destination).

## Constraints
- User decision: reassigning an already-synced project to a new remote re-enqueues the project's
  FULL history for the new remote. Nothing is deleted from the previous remote.
- Backward compatible: existing installs without `projects` behave exactly as today.
- Tokens persisted in `cloud.json` (mode 0600), never printed in full.
- Artifacts in English. Conventional commits, no AI attribution.

## Tasks
- [x] T1 — `cloudconfig`: per-project map, load/save, validation, `ResolveForProject`, remote id,
  env override semantics; unit tests.
- [x] T2 — (carries T1 review follow-ups: validate loaded overrides in `ResolveForProject`
  — empty/invalid `server_url` must surface an error instead of a silent unusable remote;
  trim the global URL in `resolveGlobal`; normalize project names before lookup.) CLI: `engram cloud config --project X --server URL --token T` / `--project X --clear`;
  `engram sync --cloud --project X` + upgrade paths use per-project resolution; reassignment
  re-enqueues full project history; `cloud status` shows per-project remotes; tests.
- [x] T3 — (carries T2 review follow-up: `cloud config --project` saves cloud.json before
  requeue; if requeue fails the new remote stays saved and re-running reports "unchanged", so
  history never reaches the new remote — restore the previous config on requeue failure, with a
  test. Also: `cloudSyncEnabled` status provider must resolve per project.) Autosync: one manager per remote with separate state key (`cloud@<id>`) for cursor/lease,
  project-filtered push/pull, global manager excludes routed projects, status adapter routes by
  project; tests.
- [ ] T4 — (carries T3 review follow-ups: (a) pull catch-up when a project's route changes —
  scoped managers advance their cursor past out-of-scope rows, so routing a project back to a
  remote misses rows other devices wrote meanwhile; (b) `autosyncGroup` StopForUpgrade/Resume must
  be a no-op, not an error, for unowned projects when only overrides are configured; (c) test the
  env-only global setup (no cloud.json); (d) restore `storeNew` via t.Cleanup in the routing test;
  (e) align tokenless-override handling between autosync (skips) and explicit sync.) Doctor/cleanup recognizes `cloud@*` keys and prunes orphaned `cloud@*` rows whose id
  no longer matches any configured remote (token rotation changes `RemoteID`; accepted — a fresh
  pull from seq 0 is safe); docs for the new config; tests.

## Acceptance criteria
- A project with an override pushes/pulls only against its remote; others only against global.
- Two remotes keep independent pull cursors and leases.
- Reassignment delivers full history to the new remote.
- Doctor does not retarget or delete `cloud@*` state.
- `go test ./...` green.

## Checks
- Focused `go test` per package touched; `go test ./...` at task closure; `go vet ./...`.

## Delivery
- Forecast: ~1000+ authored changed lines (exceeds ~400 budget). Strategy: ask-on-risk → user chose `feature-branch-chain`
  (task PRs target integration branch `feat/per-project-cloud-remote`; one final PR to main).
- Slices: one task branch/PR per task (T1..T4); boundaries recorded below as commits land.

## Progress / evidence
| Task | Route | Commit | Checks | Review |
|------|-------|--------|--------|--------|
| T1 | delegated (writer trigger: config.go + new remote.go + tests) | 969cfa2 | RED: build fail `got.Projects undefined`; GREEN: `go test ./internal/cloudconfig/...` ok, `go vet` clean, `go build ./...` ok; parent spot check re-ran go test ok | medium, granted; 1 lens (reliability) approved, acknowledged (authority burned). 3 advisory findings moved to T2/T4. Reviewed boundary → 969cfa2 |

Note: first RDD preflight defaulted to base 2e28f19 (whole prior branch, ~4.1k lines) and stopped with
`lens_context_budget_exceeded`; rescoped to this feature's branch point 6961d7e.
| T2 | delegated (writer trigger: cloud.go, main.go, store.go, cloudconfig + tests) | a14397e | RED: 8 cmd failures + build fails in cloudconfig/store; GREEN: cloudconfig ok, store ok, `go vet ./...` ok, `go build ./...` ok; cmd/engram ok with `ENGRAM_CLOUD_AUTOSYNC` unset (4 `TestCmdServe*SyncStatus*` fail only when the ambient env var is 1, also on base 9f3101a); parent spot check re-ran focused tests ok | medium, granted; reliability lens approved + acknowledged; 1 advisory WARNING moved to T3. Reviewed boundary → a14397e |

T2 notes: "full history" = current-state replay via the enroll/remirror backfill path under a unique
`reassign:<nanos>` source (not every historical revision). The cloud server appends duplicate
`cloud_mutations` rows on re-push (cloudstore.go:941), while chunks dedupe by content hash
(cloudstore.go:962-964). Token rotation on the same server counts as a remote change and requeues.
| T3 | delegated (writer trigger: manager.go, store.go, main.go, autosync_status.go, cloud.go + tests) | de172a5 | RED: build fails (StateKey/Scoped/autosyncGroup undefined); GREEN (writer): autosync/store/cloudconfig ok, cmd/engram ok (139s), vet ok, build ok, all with `ENGRAM_CLOUD_AUTOSYNC` unset; parent spot check: autosync + cmd routing tests ok | medium, granted; reliability lens approved + acknowledged; 2 WARNING + 2 SUGGESTION moved to T4. Reviewed boundary → de172a5 |

T3 notes: 1336 changed lines (~770 tests) — over the advisory budget; not split because store query,
manager scoping and wiring are one coherent behavior. Windows `UnixNano` resolution made two managers
share a LeaseOwner; remote managers now get an id suffix.
