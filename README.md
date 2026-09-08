# Construct Graph

Backend-as-a-Service for Construct spaces. Data engine with automatic GraphQL API, schema registration, install / distribution gates, and per-row access enforcement. SQLite for local dev, MySQL / PostgreSQL for production.

> **Public surface is intentionally minimal: `POST /graphql` (runtime) + `GET /realtime/stream` (events) + `GET /health`.** Every schema-management and admin operation comes in through [`developer-api`](https://github.com/construct-space/developer-api), which forwards to graph internally over the swarm with `X-Internal-Secret` + `X-Auth-*` headers. Browser traffic to the root is redirected to `my.lisaos.dev`.

## Prerequisites

- **Go 1.26+** — [go.dev/dl](https://go.dev/dl/)
- **MySQL 8+ or PostgreSQL 15+** — production. Local dev uses SQLite by default.

### Install Go

**macOS:**
```bash
brew install go
```

**Linux:**
```bash
wget https://go.dev/dl/go1.26.0.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf go1.26.0.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc
```

**Verify:**
```bash
go version
```

## Setup

```bash
git clone git@github.com:construct-space/graph.git
cd graph
go mod download
```

## Run (Local Dev)

No database setup required — defaults to SQLite:

```bash
go run ./cmd/graph
```

This creates `data/graph.db` automatically and starts on `http://localhost:8080`.

## Run (Production)

```bash
# MySQL (default for the deployed fleet)
DATABASE_URL="mysql://user:pass@tcp(host:3306)/construct_graph?parseTime=true" go run ./cmd/graph

# PostgreSQL
DATABASE_URL="postgres://user:pass@host:5432/construct_graph?sslmode=disable" go run ./cmd/graph
```

## Configuration

All config is via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `DATABASE_URL` | `data/graph.db` (SQLite) | Connection string. `mysql://`, `postgres://`, file path, or `sqlite://path` |
| `PORT` | `8080` | HTTP server port |
| `INTERNAL_SHARED_SECRET` | (unset) | Required in prod. Validates internal calls from `developer-api` and `oracle-api`. |
| `ACCOUNTS_URL` | `http://srv-captain--accounts` | Accounts service root (token validation) |
| `DEV_PORTAL_URL` | `http://srv-captain--developer` | Developer service root (publisher API key validation) |

### Database URL Examples

```bash
# SQLite (local dev)
DATABASE_URL="data/graph.db"
DATABASE_URL="sqlite:///tmp/graph.db"

# MySQL (production fleet default)
DATABASE_URL="mysql://user:pass@tcp(db.example.com:3306)/graph?parseTime=true"

# PostgreSQL
DATABASE_URL="postgres://user:pass@db.example.com:5432/graph?sslmode=require"
```

## API Surface

Routes are split into three tiers — what's reachable from the public domain (`graph.lisaos.dev`), what flows in from `developer-api` over the internal swarm overlay, and what `oracle-api` (admin) calls with the internal secret.

### Public — runtime data plane

| Method | Path | Notes |
|---|---|---|
| `GET` | `/health`, `/api/health` | Liveness probe |
| `GET` | `/graphql` | GraphQL Playground |
| `POST` | `/graphql` | GraphQL queries + mutations. Requires `Authorization: Bearer <token>`, `X-Space-ID`, `X-Project-ID`. |
| `GET` | `/realtime/stream` | Server-sent events for subscription updates |
| `GET` | `/` | Redirect to `https://my.lisaos.dev` |

### Façade-only — internal calls from `developer-api`

These routes still exist on graph but are not exposed on the public domain. Clients call the developer-api equivalent at `https://my.lisaos.dev/api/developer/<path>`, which proxies here with `X-Internal-Secret` + identity headers.

| Method | Graph path | Developer façade | Description |
|---|---|---|---|
| `POST` | `/api/schemas/register` | `POST /api/schemas/register` | Register a space schema (CLI `construct graph push`) |
| `GET` | `/api/schemas/{spaceId}` | `GET /api/schemas/{spaceId}` | Fetch a space's models + fields |
| `DELETE` | `/api/schemas/{spaceId}` | `DELETE /api/schemas/{spaceId}` | Drop a schema (cascade — schemas + manifests + spaces row) |
| `GET` | `/api/spaces/{spaceId}/tables/{tableName}/rows` | `GET /api/schemas/{spaceId}/tables/{tableName}/rows` | Owner-scoped data browse (paginated). Server enforces ownership against `X-Auth-*`. Optional `?project=<id>` to disambiguate when a space has multiple project-scoped schemas. |
| `GET` | `/api/spaces` | — | Publisher dashboard list (org's spaces with bundle / distribution / install count) |
| `POST` / `GET` / `GET {id}` | `/api/space-bundles` | — | Publisher bundle CRUD (org-scoped) |
| `POST` / `DELETE` | `/api/spaces/{id}/install` | — | Tenant install / uninstall (data preserved on uninstall) |
| `GET` | `/api/spaces/{id}/installs` | — | Publisher lists installing orgs |
| `PUT` | `/api/spaces/{id}/distribution` | — | Set `public` / `org_allowlist` / `private` |
| `POST` / `DELETE` | `/api/spaces/{id}/allowlist` | — | Allowlist entry management |

### Admin — `oracle-api` proxy (`X-Internal-Secret`)

`requireAdminAuth` accepts either the internal secret (oracle's path) or a logged-in admin OAuth session.

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/admin/stats` | Schema / table / row totals + DB size |
| `GET` | `/api/admin/schemas` | Every provisioned schema across all spaces |
| `GET` | `/api/admin/schemas/{spaceId}/models` | Model definitions for a space |
| `GET` | `/api/admin/schemas/{schemaName}/tables/{tableName}/rows` | Browse rows in a specific provisioned schema (paginated). Powers Oracle's data drawer + CSV export. |
| `DELETE` | `/api/admin/schemas/{name}` | Drop one schema by name |
| `GET` | `/api/admin/spaces` | List every space across all orgs |
| `DELETE` | `/api/admin/spaces/{spaceId}` | Cascade-delete: drops all schemas + manifests + spaces row. Tolerant of orphans (registered-but-never-migrated rows). |

## GraphQL Usage

Requests require `X-Space-ID` and `X-Project-ID` headers:

```bash
# Query
curl -X POST https://graph.lisaos.dev/graphql \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <token>" \
  -H "X-Space-ID: my-space" \
  -H "X-Project-ID: my-project" \
  -d '{"query":"{ employees { id name email } }"}'

# Mutation
curl -X POST https://graph.lisaos.dev/graphql \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <token>" \
  -H "X-Space-ID: my-space" \
  -H "X-Project-ID: my-project" \
  -d '{"query":"mutation { createEmployee(input:{name:\"John\",email:\"john@example.com\"}) { id } }"}'
```

## Schema Registration

The CLI's `construct graph push` and `my.lisaos.dev`'s schema editor both call into developer-api, which proxies to this service. End-to-end:

```bash
curl -X POST https://my.lisaos.dev/api/developer/schemas/register \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer cat_..." \
  -H "X-API-Key: csk_live_..." \
  -d '{
    "space_id": "my-space",
    "space_name": "My Space",
    "version": "1.0.0",
    "project_id": "project-1",
    "manifest": {
      "version": 1,
      "models": [
        {
          "name": "employee",
          "fields": [
            {"name": "name", "type": "string", "required": true},
            {"name": "email", "type": "string", "unique": true},
            {"name": "role", "type": "enum", "values": ["admin", "member", "viewer"]},
            {"name": "active", "type": "boolean", "default": true},
            {"name": "metadata", "type": "json"}
          ]
        }
      ]
    }
  }'
```

When a manifest is registered with new fields, tables are migrated automatically (additive-only — columns are added, never removed).

## Publisher Bundles and Distribution

Spaces can be grouped into **bundles** — a publisher-side identity that links related spaces (e.g. a "Kanban Suite" containing `kanban` and `kanban-admin`) under one ownership umbrella. Once grouped:

- Spaces in the same bundle can **import** models from each other (cross-schema at runtime, validated at publish).
- A new access level, `publisher_admin`, lets admin spaces read across every tenant of a sibling space while still gating access to the publishing org's members.
- A built-in `_installs` GraphQL query exposes the org ids that installed a sibling — publisher dashboard use only.

Tenants install each space explicitly before they can query it. Three distribution modes:

- `public` (default) — any org can install
- `org_allowlist` — only orgs in `_system.space_allowlist` may install
- `private` — only the publisher org may install (useful for admin spaces)

The GraphQL endpoint enforces the install gate: for any space with a `publisher_org_id`, a non-publisher caller must have a row in `_system.space_installs`, else 403.

See [docs/usage/bundles.md](./docs/usage/bundles.md) for the full lifecycle walkthrough (create bundle → publish app + admin → mark admin private → tenant installs → publisher queries `_installs`).

## Access Levels

Per-operation rules live on each model via `options.access`:

| Level | Who is allowed |
|---|---|
| `public` | Anyone, no auth required |
| `authenticated` | Any logged-in user |
| `owner` | Only the row's creator (`created_by = callerUserID`) |
| `member` | Any org member of the current scope |
| `admin` | Authenticated (not yet enforced beyond auth) |
| `publisher_admin` | Caller's org must equal the space's `publisher_org_id` AND hold `owner`/`admin`/`developer` role. Bypasses row-level tenant filters. |
| `none` | Operation blocked entirely |

## Docker

```bash
docker build -t construct-graph .

# MySQL
docker run -p 8080:8080 \
  -e DATABASE_URL="mysql://user:pass@tcp(host:3306)/graph?parseTime=true" \
  -e INTERNAL_SHARED_SECRET="..." \
  construct-graph

# SQLite (ephemeral — use a volume for persistence)
docker run -p 8080:8080 -v graph-data:/data construct-graph
```

## Project Structure

```
cmd/graph/           Entry point — only binary the service ships
internal/
  auth/              Token validation against accounts
  gwauth/            Gateway auth middleware (X-Auth-* + X-Internal-Secret)
  engine/            Query engine (GORM — supports SQLite, MySQL, PostgreSQL)
  graphql/           GraphQL handler + playground + realtime hub
  schema/            Schema registry (GORM migrator) + admin handlers + bundles
docs/
  superpowers/       Architecture, design notes, decision records
  usage/             Lifecycle walkthroughs (bundles, install gates, …)
data/                SQLite database (created automatically, gitignored)
```

## Supported Field Types

| Type | MySQL | PostgreSQL | SQLite | Description |
|------|-------|-----------|--------|-------------|
| `string` | `VARCHAR(255)` / `TEXT` | `TEXT` | `TEXT` | Text/string values |
| `int` | `INT` | `INTEGER` | `INTEGER` | Integer numbers |
| `number` | `DECIMAL` | `NUMERIC` | `REAL` | Decimal numbers |
| `boolean` | `TINYINT(1)` | `BOOLEAN` | `INTEGER` | True/false (0/1 in SQLite/MySQL) |
| `date` | `DATETIME` | `TIMESTAMPTZ` | `DATETIME` | Date/time values |
| `enum` | `VARCHAR` + CHECK | `TEXT` + CHECK | `TEXT` + CHECK | Constrained string values |
| `json` | `JSON` | `JSONB` | `TEXT` | JSON data |
| `relation` | `CHAR(36)` + FK | `UUID` + FK | `TEXT` + FK | Foreign key reference |
