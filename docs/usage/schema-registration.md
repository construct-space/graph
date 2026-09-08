# Schema Registration

Schema registration is how Graph learns about a space's data models and creates the corresponding database tables.

> **Production calls go through `developer-api` at `https://my.lisaos.dev/api/developer/...`.** It forwards to graph internally over the swarm with `X-Internal-Secret` + `X-Auth-*` headers. Paths below are the internal graph routes; flip the prefix to `https://my.lisaos.dev/api/developer/` when calling from outside the cluster.

## How It Works

When a space is published via `construct publish`, the developer portal sends the manifest to Graph's registration endpoint. Graph then:

1. **Records the space** in `_system.spaces`
2. **Stores the manifest** in `_system.manifests` (versioned)
3. **Creates a database schema** (Postgres / MySQL / SQLite — whichever the deployment uses) for the space+project combination
4. **Creates/migrates tables** for each model
5. **Records the provision** in `_system.provisions`

## Registration Endpoint

```
POST /api/schemas/register
```

```json
{
  "space_id": "canvas",
  "space_name": "Canvas",
  "project_id": "default",
  "version": "1.0.0",
  "manifest": {
    "version": 1,
    "models": [
      {
        "name": "card",
        "fields": [
          { "name": "title", "type": "string", "required": true },
          { "name": "color", "type": "string" }
        ]
      }
    ]
  }
}
```

Response:
```json
{
  "ok": true,
  "schema_name": "s_canvas_p_default",
  "tables": 1,
  "registered": "2026-03-30T00:00:00Z"
}
```

## Schema Naming

| Scope | Pattern | Example |
|-------|---------|---------|
| Project (default) | `s_{spaceId}_p_{projectId}` | `s_canvas_p_default` |
| Company | `c_{companyId}_s_{spaceId}` | `c_acme_s_canvas` |

## Table Creation

Each model becomes a table in the schema. Graph generates DDL like:

```sql
CREATE SCHEMA IF NOT EXISTS s_canvas_p_default;

CREATE TABLE IF NOT EXISTS s_canvas_p_default.card (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  title TEXT NOT NULL,
  color TEXT,
  created_at TIMESTAMPTZ DEFAULT now(),
  updated_at TIMESTAMPTZ DEFAULT now(),
  created_by UUID
);
```

## Migrations

Graph handles additive migrations automatically:

- **New fields** → `ALTER TABLE ADD COLUMN`
- **New indexes** → `CREATE INDEX IF NOT EXISTS`
- **New tables** → `CREATE TABLE IF NOT EXISTS`

Destructive changes (dropping columns, renaming) are not auto-migrated.

## Inspecting Schemas

### Get schema info

```
GET /api/schemas/{spaceId}
```

Returns models and their fields for a space.

### List provisioned spaces

```
GET /api/spaces
```

Returns all spaces with their provision status, model count, and project count.

## System Tables

Graph stores metadata in the `_system` schema:

| Table | Purpose |
|-------|---------|
| `_system.spaces` | Space registry (id, name, version) |
| `_system.manifests` | Manifest history (space_id, version, manifest JSON) |
| `_system.provisions` | Space+project provisions (schema name, version) |

## CLI Registration

The `construct graph push` command registers models with Graph without a full publish:

```bash
construct graph push
```

This is useful during development to test schema changes before publishing.
