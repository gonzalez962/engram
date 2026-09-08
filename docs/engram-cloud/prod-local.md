[← Back to Engram Cloud](./README.md)

# Production-like Local Compose (managed principals)

This page documents an isolated, **authenticated** local cloud stack that
builds from the current working tree (`docker/cloud/Dockerfile`) instead of
pulling the GHCR image. Use it to exercise managed-admin features
(`POST /admin/projects`, auto-grants, `cloud_principals` audit) on the exact
bytes you intend to deploy — without standing up Dokploy/Coolify.

This stack is intentionally **separate** from every other Engram Docker
deployment:

| File | Purpose | Auth mode |
|---|---|---|
| `docker-compose.cloud.yml` | Insecure smoke | `ENGRAM_CLOUD_INSECURE_NO_AUTH=1` |
| `docker-compose.beta.yml` | Community beta (Phases 2-4) | Legacy env-token |
| `docker-compose.prod-local.yml` (this file) | **Production-like local** | **Authenticated + managed tokens** |
| `docs/engram-cloud/docker-compose.ghcr.yml` | Production deploy | Authenticated (GHCR image) |

All four coexist on one host: disjoint ports, container names, and Postgres
volumes.

---

## Ports

| Stack | Postgres host port | Cloud host port |
|---|---|---|
| `docker-compose.cloud.yml` | `127.0.0.1:5433` | `127.0.0.1:18080` |
| `docker-compose.beta.yml` | `127.0.0.1:25432` | `127.0.0.1:28080` |
| **`docker-compose.prod-local.yml`** | **`127.0.0.1:35432`** | **`127.0.0.1:38080`** |
| `docker-compose.ghcr.yml` (production) | host-default | host-default |

All host-side bindings are `127.0.0.1` only — never published on the LAN.

---

## 1. Create a `.env` file

Never commit a real `.env`. Start from the template at the repo root:

```bash
cp .env.example .env
```

`.env` is in `.gitignore`. Replace every `replace-with-*` placeholder with a
strong, unique secret before starting the stack.

### Generating secrets on PowerShell (Windows)

```powershell
function New-EngramSecret([int]$Bytes = 32) {
    $b = New-Object byte[] $Bytes
    [System.Security.Cryptography.RandomNumberGenerator]::Fill($b)
    [Convert]::ToBase64String($b)
}

# Replace every replace-with-* placeholder in place:
(Get-Content .env) | ForEach-Object {
    if ($_ -match '^(.*?)=(.*)$') {
        $key = $matches[1]
        $val = $matches[2]
        if ($val -like 'replace-with-*') {
            "$key=$((New-EngramSecret) -replace '=+$','')"
        } else {
            $_
        }
    } else {
        $_
    }
} | Set-Content .env
```

### Generating secrets on macOS / Linux

```bash
openssl rand -base64 32
```

The four secrets **must** be distinct from each other. In particular
`ENGRAM_CLOUD_TOKEN_PEPPER` must be different from `ENGRAM_JWT_SECRET`
(see `internal/cloud/config.go` `TokenPepper` contract).

---

## 2. Start the stack

```bash
docker compose -f docker-compose.prod-local.yml --env-file .env up -d
```

This builds the image from the working tree (no GHCR pull) and starts
Postgres + `engram cloud serve` on the loopback-only ports above. Verify:

```bash
curl -s http://127.0.0.1:38080/health
# Expected: {"status":"ok","service":"engram-cloud"}
```

The server refuses to start unless **all** of the following are present and
non-default: `ENGRAM_JWT_SECRET`, `ENGRAM_CLOUD_TOKEN`, `ENGRAM_CLOUD_ADMIN`,
`ENGRAM_CLOUD_TOKEN_PEPPER`, `ENGRAM_CLOUD_ALLOWED_PROJECTS`. See
`validateCloudServeAuthConfig` in `cmd/engram/cloud.go` for the exact
contract.

---

## 3. Bootstrap the first managed admin

`engram cloud serve` in this stack starts in **authenticated mode** — no
insecure mode is involved — so the first admin must be created via the
`engram cloud bootstrap admin` CLI. The CLI connects to Postgres directly
using the same DSN/pepper/JWT secret the server uses, so load the env file
into the host shell first.

### PowerShell

```powershell
# Load every .env entry into the current process.
Get-Content .env | ForEach-Object {
    if ($_ -match '^(.*?)=(.*)$') {
        [System.Environment]::SetEnvironmentVariable(
            $matches[1], $matches[2], 'Process')
    }
}

# Run the bootstrap. The CLI prints the raw managed token EXACTLY ONCE.
go run ./cmd/engram cloud bootstrap admin `
    --username admin `
    --grant-project bootstrap-only `
    --issue-token bootstrap-token
```

### Bash / Zsh

```bash
set -a
. ./.env
set +a

go run ./cmd/engram cloud bootstrap admin \
    --username admin \
    --grant-project bootstrap-only \
    --issue-token bootstrap-token
```

Or with a prebuilt `engram` on your PATH:

```bash
engram cloud bootstrap admin --username admin --grant-project bootstrap-only --issue-token bootstrap-token
```

Capture the printed `egc_live_...` token. This is the only credential that
can call `POST /admin/projects`.

> `--grant-project` may repeat. Managed principals are deny-by-default (no
> grants means no sync access). `--issue-token [name]` prints the raw
> managed token exactly once and requires `ENGRAM_CLOUD_TOKEN_PEPPER` to be
> set to a dedicated secret, distinct from `ENGRAM_JWT_SECRET`. Every
> bootstrap attempt — accepted or refused — is recorded as a `bootstrap.cli`
> audit event. See `cmd/engram/cloud_bootstrap.go`.

---

## 4. Test `POST /admin/projects`

```bash
curl -i -X POST http://127.0.0.1:38080/admin/projects \
    -H "Authorization: Bearer $ENGRAM_BOOTSTRAP_TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"name":"my-new-project"}'
```

PowerShell equivalent:

```powershell
$headers = @{
    'Authorization' = "Bearer $env:ENGRAM_BOOTSTRAP_TOKEN"
    'Content-Type'  = 'application/json'
}
Invoke-RestMethod -Method Post `
    -Uri 'http://127.0.0.1:38080/admin/projects' `
    -Headers $headers `
    -Body '{"name":"my-new-project"}'
```

Expected: `201 Created` with `{"name":"my-new-project","sync_enabled":true}`.

The acting admin receives a grant on `my-new-project` automatically (via
`cloud_project_grants`) and the call writes a `project.create` audit event —
all atomic in one transaction. Subsequent Cloud Sync runs that
authenticate with the same managed token can now push/pull against
`my-new-project`.

The executable API contract for this endpoint is documented in the
[`docs/bruno/engram-cloud-self-hosted/Admin/11-create-project.yml`](../bruno/engram-cloud-self-hosted/Admin/11-create-project.yml)
Bruno request (with response envelope, status codes, and atomicity
semantics).

---

## 5. Role of `ENGRAM_CLOUD_ALLOWED_PROJECTS`

`ENGRAM_CLOUD_ALLOWED_PROJECTS` is **required** for startup validation
(`validateCloudServeAuthConfig` rejects a token-mode server with an empty
allowlist). It is, however, **only consulted by the legacy env-token
authentication path** (`internal/cloud/auth.ProjectScopeAuthorizer`).

Managed principals authenticate as `PrincipalSourceManagedToken` and
authorize projects via `cloud_project_grants`. They ignore this allowlist
entirely — see `usesManagedProjectGrants` in
`internal/cloud/cloudserver/cloudserver.go`. So setting this to a benign
non-empty value (e.g. `bootstrap-only`) is sufficient to satisfy the
startup check while the production authorization path is governed by
per-principal grants managed through the admin API.

A project created via `POST /admin/projects` does **not** modify
`ENGRAM_CLOUD_ALLOWED_PROJECTS`. It is reachable only through the acting
admin's managed token + the auto-issued grant. To grant a non-admin
managed token access to the project, run `POST /admin/users/{id}/grants`
or use the dashboard.

---

## 6. Cleanup

```bash
docker compose -f docker-compose.prod-local.yml --env-file .env down -v
```

This stops the containers and **destroys** the `engram-prod-local-pg`
volume. The smoke and beta stacks are untouched.

```bash
# Remove the local .env when you no longer need the stack.
rm -f .env
```

---

## 7. Validation without a full E2E

The compose file is statically validated by:

```bash
docker compose -f docker-compose.prod-local.yml --env-file <ephemeral env> config
```

A short shell snippet that produces a disposable env without committing
real secrets:

```bash
TMP=$(mktemp)
grep -E '^[A-Z0-9_]+=replace-with-' .env.example | sed 's/=.*$/=ephemeral-static-check/' > "$TMP"
docker compose -f docker-compose.prod-local.yml --env-file "$TMP" config > /dev/null
rm -f "$TMP"
```

If the command exits non-zero, the compose file has a structural problem
(interpolation, missing required env, malformed port mapping, etc.).

---

## Troubleshooting

**Server exits with `invalid ENGRAM_CLOUD_TOKEN_PEPPER`**
The pepper must be at least 32 bytes; very short strings fail the
`auth.NewManagedTokenHasher` length check.

**Server exits with `authenticated cloud serve requires a non-default ENGRAM_JWT_SECRET`**
`ENGRAM_JWT_SECRET` is empty or still equals the development default
`engram-dev-jwt-secret-for-local-smoke-1234`. Replace it.

**`engram cloud bootstrap admin` errors with `ENGRAM_CLOUD_TOKEN_PEPPER environment is not set`**
The env file is not loaded in the host shell. Re-run step 3 with the env
loaded (`set -a; . ./.env; set +a` on bash, or the `Get-Content` loop on
PowerShell) — or pass the values as inline env vars to the command.

**`POST /admin/projects` returns `403` with `forbidden: managed admin principal is required`**
The bearer token is not a managed admin token. The bootstrap command
prints the token once; if you lost it, mint a new admin token via
`POST /admin/users/{principalID}/tokens` (requires the existing managed
admin token).

**Port already in use**
Pick a different host port in `docker-compose.prod-local.yml` and re-run
`docker compose … up -d`. Update the local `.env` accordingly if you
change `POSTGRES_USER`/`POSTGRES_DB`.
