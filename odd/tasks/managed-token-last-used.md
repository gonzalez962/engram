# Feature: record managed token last use

Locator: `odd/tasks/managed-token-last-used.md` · Engram mirror: `odd/managed-token-last-used/tasks`
Branch: `fix/managed-token-last-used` (from `origin/main` @ 91e2d2b)

## Objective
The admin dashboard must show when each managed token (including the bootstrap admin token) was
last used.

## Problem / why
- `cloud_principal_tokens.last_used_at` exists (`internal/cloud/cloudstore/cloudstore.go:673`), is
  read (`identity.go:714`, `identity.go:762`) and rendered (`internal/cloud/dashboard/components.templ:1080`),
  but no production code ever writes it; only tests set it (`identity_storage_test.go:414`, `:557`).
- Managed token authentication (`cmd/engram/cloud_runtime_auth.go:79` `FindManagedTokenByHash`)
  only reads the token.
- Side effect: the stranded-admin-token replacement guard (`identity.go:580`, "never used") is
  currently always satisfied; after this fix a used token is no longer replaceable, as designed.

## Constraints
- Record usage only after a successful authentication (token valid, not revoked, principal enabled).
- Throttle writes: at most one update per token per minute (conditional UPDATE), no write on every request.
- A failure to record usage must not fail the request; log it.

## Tasks
- [x] T1 — Store method to touch `last_used_at` (throttled) + call it on successful managed token
  authentication (API bearer and dashboard login/session revalidation paths), with tests.
  Commit: `6bce248` (`fix(cloud): record managed token last use`). Route: delegated direct (one writer).

## Route
Delegated direct: one writer (store + auth wiring + tests across packages).

## Acceptance criteria
- After a successful request with a managed token, the token's `last_used_at` is set and visible
  in the admin user detail.
- Repeated requests within a minute do not issue additional writes.
- Revoked/unknown tokens and disabled principals do not update `last_used_at`.

## Checks
- `go build ./...`
- `go vet ./internal/cloud/... ./cmd/engram/...`
- `go test ./internal/cloud/... ./cmd/engram/ -run 'Cloud|Dashboard|Token|Auth'`

## Progress
- Feature doc created; branch created.
- T1 done (`6bce248`). Seam: optional `auth.ManagedTokenUsageRecorder` capability checked by
  `PrincipalResolver.ResolveBearerToken` only after success; every API bearer, dashboard login and
  session revalidation path resolves through it. `cloudstoreManagedTokenLookup.RecordManagedTokenUse`
  forwards to `CloudStore.TouchPrincipalTokenLastUsed` (conditional UPDATE: not revoked, principal
  enabled, `last_used_at` NULL or older than 1 minute). Failures are logged, never fail the request.
  No `cloudserver` changes were needed.

## Verification evidence (T1)
- RED observed: auth resolver tests (no recording), cmd/engram adapter tests (missing capability),
  cloudstore tests and compile-time assertion (missing `TouchPrincipalTokenLastUsed`).
- GREEN observed; mutation check (removing revoked/enabled guards) made the cloudstore test fail.
- `go build ./...`: ok
- `go vet ./internal/cloud/... ./cmd/engram/...`: ok
- `go test ./internal/cloud/cloudstore/ ./internal/cloud/auth/ ./internal/cloud/cloudserver/ ./internal/cloud/dashboard/ -count=1`:
  ok without DSN (DB tests skipped) and ok with `CLOUDSTORE_TEST_DSN` (temporary `postgres:16-alpine`).
- `go test ./cmd/engram/ -run 'Cloud|Dashboard|Token|Auth' -count=1`: ok without and with DSN.
- Stranded admin recovery tests (`identity_storage_test.go`) still pass unchanged.

## Next step
Native review / delivery decision for `6bce248` under ordinary repository policy.
