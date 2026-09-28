# Engram Cloud Server — Bruno OpenCollection

Executable documentation for the HTTP API exposed by a self-hosted
`engram cloud serve` instance. Every request in this collection maps 1:1
to a route registered in `internal/cloud/cloudserver/cloudserver.go`.

The collection is written in the modern Bruno OpenCollection YAML format
(v3.1+, OpenCollection spec 1.0.0). It does **not** use the legacy
`.bru` script files.

## Start the server

The collection assumes you have already started an Engram cloud server.
For a self-hosted PostgreSQL-backed dev instance:

```bash
# 1. Start Postgres on port 5433 (the Engram cloud default).
docker compose -f docker-compose.cloud.yml up -d

# 2. Configure required environment variables.
export ENGRAM_PORT=8080
export ENGRAM_CLOUD_HOST=127.0.0.1
export ENGRAM_CLOUD_ALLOWED_PROJECTS=bruno-demo
export ENGRAM_JWT_SECRET="$(openssl rand -hex 32)"
export ENGRAM_CLOUD_TOKEN_PEPPER="$(openssl rand -hex 32)"

# 3. Pick a Bearer credential.
#    Option A (legacy sync token) — sufficient for /health and /sync/*,
#    but NOT for /admin/*. Set:
export ENGRAM_CLOUD_TOKEN="$(openssl rand -hex 32)"

#    Option B (managed admin token) — required for /admin/*. Mint one:
export ENGRAM_CLOUD_ADMIN="$(openssl rand -hex 32)"  # only for dashboard cookie flow
engram cloud bootstrap admin \
    --username root \
    --email root@example.com \
    --display-name "Root Operator" \
    --issue-token
# The command prints an `egc_live_...` token. Export it as:
export ENGRAM_CLOUD_MANAGED_TOKEN="egc_live_..."

# 4. Start the server.
engram cloud serve
# -> [engram-cloud] listening on 127.0.0.1:8080
```

See `internal/cloud/config.go` (env var reference) and `cmd/engram/cloud.go`
(`validateCloudServeAuthConfig`) for the authoritative configuration
contract. The server refuses to start without either `ENGRAM_CLOUD_TOKEN`
or `ENGRAM_CLOUD_INSECURE_NO_AUTH=1`, and in both cases requires a
non-empty `ENGRAM_CLOUD_ALLOWED_PROJECTS`.

## Open the collection in Bruno

1. Launch Bruno.
2. Click **Open Collection** and select the `docs/bruno/engram-cloud-self-hosted/`
   directory of this repository.
3. Bruno reads `opencollection.yml` at the root and recognizes the
   collection automatically.

## Configure environment variables

The collection ships with `environments/Local.yml`. The required
variables are:

| Variable     | Purpose                                                                  | Source / notes                                                                          |
| ------------ | ------------------------------------------------------------------------ | --------------------------------------------------------------------------------------- |
| `baseUrl`    | `http://host:port` of the cloud server                                   | Defaults to `http://127.0.0.1:8080`. Override to match `ENGRAM_PORT` + `ENGRAM_CLOUD_HOST`. |
| `token`      | Bearer credential sent on every `/sync/*` and `/admin/*` request         | `secret: true`. For `/sync/*` use `ENGRAM_CLOUD_TOKEN`; for `/admin/*` use a managed admin token. |
| `project`    | Project name used by `/sync/*` requests and embedded in mutation batches | Must appear in `ENGRAM_CLOUD_ALLOWED_PROJECTS`.                                          |
| `principalId`| UUID of the managed user, filled in by `Admin/02-create-user`             | Captured automatically via the `after-response` script on `02-create-user`.             |
| `chunkId`    | 8-char SHA-256 prefix identifying a chunk                                | Captured automatically via the `after-response` script on `Sync/03-push-chunk`.         |
| `tokenId`    | UUID identifying a managed token                                         | Captured automatically via the `after-response` script on `Admin/06-create-user-token`. |
| `grantProject` | Project name used by `Admin/09-create-user-grant` and `Admin/10-revoke-user-grant` | Captured automatically; must match the server-normalized form.                       |
| `newUsername` / `newEmail` / `newDisplayName` / `newRole` | New human user attributes | Defaults are fixture-friendly; override before running `Admin/02-create-user`.          |
| `newTokenName` | Friendly name stored on the new managed token                          | Default: `bruno-demo-token`.                                                            |
| `newProject` | New cloud project name passed to `Admin/11-create-project`           | Default: `bruno-demo-api`. Server-normalized; the response name is captured into `project`. |
| `revokeReason` | Reason string persisted on the auth audit log on every revoke          | Default: `revoked via Bruno OpenCollection`.                                            |
| `managedRawToken` | Raw managed token captured from `Admin/06-create-user-token`       | `secret: true`. **Show-once** — the server never returns it again.                      |

Replace `token` with the real value before sending any authenticated
request. **Do not commit real tokens.**

## Safe execution order

Run requests in this order to avoid dead-ends and capture every id the
later steps need.

### 0. Confirm reachability

- `Health/01-cloud-health` — must return `200 {"status":"ok",...}`.

### 1. Sync flow

- `Sync/01-pull-manifest` — inspect the manifest (no side effects).
- `Sync/02-pull-chunk` — fetch one existing chunk by id (no side effects).
  Requires `chunkId`. If empty, run `Sync/03-push-chunk` first.
- `Sync/03-push-chunk` — uploads one synthetic chunk. Captures the
  server-canonicalized chunk id into `chunkId`.
- `Sync/04-push-mutations` — uploads one relation mutation. Captures
  the highest accepted seq into `latestMutationSeq`.
- `Sync/05-pull-mutations` — retrieves mutations, optionally from
  `since_seq=0` or from the captured `latestMutationSeq`.

### 2. Admin flow

**Requires a managed admin token in `token`**, not the legacy sync token.
Mint one with `engram cloud bootstrap admin --issue-token` before opening
the `Admin/` folder.

- `Admin/11-create-project` (optional) — creates a brand-new cloud project
  via the API. The acting admin automatically gets a grant on the new
  project. The server-normalized name is captured into `project` so the
  downstream `Sync/*` requests can push/pull against it without manual
  edits. The legacy `ENGRAM_CLOUD_ALLOWED_PROJECTS` env allowlist is NOT
  modified by this call — API-created projects are reachable through the
  admin's managed token and grant, not through the legacy sync token.
  Returns `409` if the project already exists. Run this FIRST in the
  Admin flow if you want to use a project that does not already exist in
  the legacy allowlist.
- `Admin/01-list-users` — snapshot existing users.
- `Admin/02-create-user` — creates a managed human user. Captures the
  new `principalId`.
- `Admin/03-enable-user` / `Admin/04-disable-user` — toggle the user's
  `enabled` flag. Re-enable before minting tokens (`Admin/06`) or the
  server returns 409.
- `Admin/05-list-user-tokens` — list tokens. Empty array initially.
- `Admin/06-create-user-token` — mints a new managed token. Captures the
  raw `managedRawToken` (one-time) and `tokenId`.
- `Admin/07-revoke-token` — revokes `tokenId` and writes
  `revocation_reason` on the row plus an audit event.
- `Admin/08-list-user-grants` — list grants (empty initially).
- `Admin/09-create-user-grant` — grants the user access to one project.
  Captures the server-normalized `grantProject`.
- `Admin/10-revoke-user-grant` — revokes the grant just created.

## Endpoints covered

| Method | Path                                                      | Folder       |
| ------ | --------------------------------------------------------- | ------------ |
| GET    | `/health`                                                 | `Health`     |
| GET    | `/sync/pull?project=<name>`                               | `Sync`       |
| GET    | `/sync/pull/{chunkID}?project=<name>`                     | `Sync`       |
| POST   | `/sync/push`                                              | `Sync`       |
| POST   | `/sync/mutations/push`                                    | `Sync`       |
| GET    | `/sync/mutations/pull?since_seq=&limit=`                  | `Sync`       |
| GET    | `/admin/users`                                            | `Admin`      |
| POST   | `/admin/users`                                            | `Admin`      |
| POST   | `/admin/users/{principalID}/enable`                       | `Admin`      |
| POST   | `/admin/users/{principalID}/disable`                      | `Admin`      |
| GET    | `/admin/users/{principalID}/tokens`                       | `Admin`      |
| POST   | `/admin/users/{principalID}/tokens`                       | `Admin`      |
| POST   | `/admin/tokens/{tokenID}/revoke`                          | `Admin`      |
| GET    | `/admin/users/{principalID}/grants`                       | `Admin`      |
| POST   | `/admin/users/{principalID}/grants`                       | `Admin`      |
| POST   | `/admin/users/{principalID}/grants/{project}/revoke`      | `Admin`      |
| POST   | `/admin/projects`                                         | `Admin`      |

## Endpoints intentionally excluded

These routes exist on the cloud server but are **not** part of this
collection because they are not Bearer-authenticated JSON APIs:

- `GET /dashboard/health` — unauthenticated dashboard liveness check.
  Returns `{"status":"ok","subsystem":"dashboard"}`.
- `GET /dashboard/login`, `POST /dashboard/login`, `POST /dashboard/logout` —
  HTML form-driven cookie session flow.
- `GET /dashboard/admin/...`, `POST /dashboard/admin/...` — server-rendered
  HTML admin pages, all under `/dashboard/admin/users/{principalID}/...`,
  `/dashboard/admin/tokens/{tokenID}/revoke`, and
  `/dashboard/admin/audit-log/list`. They reuse the same authorization
  and audit paths as the JSON `/admin/*` routes documented here; the
  Bruno collection covers the JSON surface only.
- `GET /dashboard/bootstrap`, `POST /dashboard/bootstrap` — legacy admin
  recovery surface (requires the `ENGRAM_CLOUD_ADMIN` cookie credential,
  not a Bearer token).
- `GET /dashboard/static/...` — static asset paths served by the dashboard.

For the HTML dashboard flow, drive it through a real browser session.

## Status code summary

For every documented endpoint the request/response contract follows the
same conventions:

| Code  | Meaning                                                                            |
| ----- | ---------------------------------------------------------------------------------- |
| 200   | Success (read or idempotent mutation).                                             |
| 201   | Resource created (`/admin/users`, `/admin/users/{id}/tokens`, `/admin/users/{id}/grants`, `/admin/projects`). |
| 400   | Malformed payload, missing required field, or validation failure (returns a structured `{error_class, error_code, error}` envelope from `writeActionableError`). |
| 401   | Missing or invalid `Authorization: Bearer <token>` header.                         |
| 403   | Bearer token is valid but does not authorize the requested project or admin action. |
| 404   | Chunk not found (`/sync/pull/{chunkID}`), or managed user/token not found (`/admin/*`). |
| 409   | Sync is paused for the requested project, the managed user is disabled, OR `POST /admin/projects` found the project already exists. |
| 413   | Payload exceeds `ENGRAM_CLOUD_MAX_PUSH_BYTES` (default 8 MiB).                     |
| 500   | Storage error or audit insert failure (admin mutations roll back on audit failure). |

## Source-of-truth pointers

The handler implementations live under:

- `internal/cloud/cloudserver/cloudserver.go` — router (`routes()` at line 202).
- `internal/cloud/cloudserver/admin_handlers.go` — `/admin/users`, `/admin/users/{id}/tokens`, `/admin/users/{id}/grants`, `/admin/projects`.
- `internal/cloud/cloudstore/project_controls.go` — `cloud_project_controls` schema and `CreateProjectWithGrantAndAudit` (atomic project + grant + audit transaction).
- `internal/cloud/cloudserver/mutations.go` — `/sync/mutations/push`, `/sync/mutations/pull`.
- `internal/cloud/cloudserver/cloudserver.go` (`handlePushChunk`,
  `handlePullManifest`, `handlePullChunk`, `handleHealth`) — chunk sync.
- `internal/cloud/auth/foundation.go` — managed token format
  (`egc_live_<8-hex>_<base64url>`) and resolver.

If you change a route, payload, or status code in any of those files,
update the matching `*.yml` request in this collection in the same PR
(see `skills/server-api/SKILL.md`).
