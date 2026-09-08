# Space Bundles

A **space bundle** groups related spaces published by the same organization under one ownership umbrella. It is the boundary for cross-space imports and publisher-admin access.

> **Naming note:** internally the grouping table is `_system.space_bundles` and the column on `spaces` is `bundle_id`. The word "project" is already used in this service for the consumer-side tenant workspace (`_system.provisions.project_id`, the `p_<tenant>` portion of schema names), so the publisher grouping takes a distinct name.

> **Endpoint paths in this doc are graph's internal routes.** Production traffic goes through `developer-api` at `https://my.lisaos.dev/api/developer/...`, which forwards to graph with `X-Internal-Secret` + `X-Auth-*` headers. Flip the prefix when calling from outside the cluster.

## Canonical example

Flak's org publishes a Kanban product. It ships as two spaces:

| Space | Role | Distribution |
|---|---|---|
| `kanban` | The app that tenants (basecode, urbanway) install and use. | `public` |
| `kanban-admin` | Flak's own admin UI — lists installs, reads aggregates across tenants. | `private` |

Both share `bundle_id = "kanban-suite"` and `publisher_org_id = "org-flak"`.

## Data model

```sql
_system.space_bundles (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  owner_org_id TEXT NOT NULL,
  created_at TIMESTAMPTZ DEFAULT now()
);

-- on _system.spaces
bundle_id TEXT,          -- FK to space_bundles.id (informal — not enforced)
publisher_org_id TEXT,   -- org that owns the space
```

A space may have `bundle_id = NULL` (standalone / legacy) or belong to exactly one bundle. A bundle can contain many spaces.

## HTTP API

All endpoints require an authenticated org context (`X-Auth-Org-ID` set by the auth middleware from accounts `/me/scope`).

```
POST   /api/space-bundles                { "id", "name" }   # create
GET    /api/space-bundles                                    # list owned by caller's org
GET    /api/space-bundles/{bundleId}                         # read a single bundle
```

**Create** inserts a row with `owner_org_id = callerOrg`. **Read / list** filter to the caller's org. A 403 is returned if an org tries to read a bundle it doesn't own.

## Attaching a space to a bundle

Add `bundle_id` to the space's `data.manifest.json`:

```json
{
  "version": 1,
  "bundle_id": "kanban-suite",
  "models": [ ... ]
}
```

At publish, the registry checks:

1. `bundle_id` exists in `_system.space_bundles`.
2. `space_bundles.owner_org_id == callerOrg`.

Failure modes:

| Condition | Error | Status |
|---|---|---|
| `bundle_id` not found | `bundle "..." does not exist` | 400 |
| Bundle owned by another org | `bundle "..." is owned by a different org` | 403 |
| No org context on call | `bundle_id requires authenticated org context` | 400 |

## Cross-space imports

A space in the bundle can pull models from a sibling space by declaring `imports` in its manifest:

```json
{
  "version": 1,
  "bundle_id": "kanban-suite",
  "imports": [
    { "from": "kanban", "models": ["board", "card"] }
  ],
  "models": [
    {
      "name": "board",
      "fields": [],
      "options": { "access": { "read": "publisher_admin" } }
    }
  ]
}
```

### Publish-time validation

For each import, the registry asserts `imports[i].from.bundle_id == self.bundle_id`. Cross-bundle imports are rejected with `source space "..." is in a different bundle`.

Rows in `_system.space_imports(space_id, from_space_id, model_name)` record the resolved mapping. Re-publishing overwrites them — the manifest is the source of truth.

### Runtime resolution

When a resolver receives a query for model `board` in space `kanban-admin`, the registry's `ResolveModel` performs:

1. Look up `board` in `kanban-admin`'s manifest.
2. If not found, look up `kanban-admin` → `board` in `_system.space_imports`; that points at `kanban`.
3. Return `{ SourceSpaceID: "kanban", AccessSpaceID: "kanban-admin", Model: <from kanban> }`.

The handler emits SQL against `kanban`'s physical schema (the tenant's `s_kanban_p_<tenant>`) but looks up access rules on `kanban-admin`'s manifest. This lets the admin space override access without moving tables.

### Access rules

If the importer re-declares the model (as in the example), its `access` rules apply. If the importer only lists the model in `imports` without a `models` entry, access falls back to the source space's rules.

## `publisher_admin` access level

A new level between `admin` and `owner`:

| Level | Meaning |
|---|---|
| `publisher_admin` | Caller's `X-Auth-Org-ID` must equal `spaces.publisher_org_id`. Row-level tenant filters (`created_by`, `org_id`) are **suppressed** — the caller sees all rows across tenants. |

This is what makes `kanban-admin` useful: reading `board` with `read: publisher_admin` returns every board from every tenant install, restricted to the Flak org only.

> **Future hardening**: currently the check is org-match only. A TODO exists to also require the `admin` role within that org (via accounts `/api/orgs/:id/members/:uid`). Until then, `distribution=private` on admin spaces ensures only publisher-org members reach these resolvers.

## Built-in `_installs` query

Publisher-admin spaces can list the orgs that installed a sibling space:

```graphql
query {
  _installs(spaceId: "kanban") {
    orgId
    spaceId
  }
}
```

The `_` prefix marks it as a platform query (not backed by a user-defined model). Enforcement:

1. Caller's space must have `publisher_org_id != ""`.
2. Caller's `X-Auth-Org-ID` must match that `publisher_org_id`.
3. If `spaceId` argument differs from the current space, target must share the same `bundle_id`.

## Distribution and installs

Three distribution modes control who may install a space:

| Mode | Behavior |
|---|---|
| `public` (default) | Any org can install. |
| `org_allowlist` | Only orgs present in `_system.space_allowlist` may install. |
| `private` | Only the publisher org may install. |

The publisher org is implicitly installed regardless of mode.

### Endpoints

```
POST   /api/spaces/{spaceId}/install      # tenant installs
DELETE /api/spaces/{spaceId}/install      # tenant uninstalls
GET    /api/spaces/{spaceId}/installs     # publisher lists installs
PUT    /api/spaces/{spaceId}/distribution # publisher changes mode
POST   /api/spaces/{spaceId}/allowlist    # publisher adds org to allowlist
DELETE /api/spaces/{spaceId}/allowlist    # publisher removes org
```

`InstallSpace` is idempotent. Uninstall leaves tenant data intact — only the install row is removed — so reinstalling reconnects seamlessly.

### Install gate at `/graphql`

Every GraphQL request to an owned space (space has `publisher_org_id` set) requires the caller's org to have an install row, unless the caller is the publisher. Missing install → 403 with a pointer to the install endpoint. Legacy standalone spaces (no `publisher_org_id`) skip this gate for backward compatibility.

## End-to-end flow

```text
1. Flak's org creates a bundle:
     POST /api/space-bundles  { id: "kanban-suite", name: "Kanban Suite" }

2. Flak publishes the app space:
     manifest: { bundle_id: "kanban-suite", ... }
     → _system.spaces row created with publisher_org_id=org-flak

3. Flak publishes the admin space:
     manifest: {
       bundle_id: "kanban-suite",
       imports: [{ from: "kanban", models: ["board"] }],
       models: [{ name: "board", ..., options: { access: { read: "publisher_admin" } }}]
     }

4. Flak marks admin space private:
     PUT /api/spaces/kanban-admin/distribution  { distribution: "private" }

5. Basecode org installs the app space:
     POST /api/spaces/kanban/install  (with X-Auth-Org-ID: org-basecode)

6. Basecode user queries as usual:
     POST /graphql  (X-Space-ID: kanban, X-Auth-Org-ID: org-basecode)
     → owner filter applied; reads only basecode rows

7. Flak queries admin space:
     POST /graphql  (X-Space-ID: kanban-admin, X-Auth-Org-ID: org-flak)
     Query: { boards { id title created_by } }
     → tenant filter skipped (publisher_admin), reads all orgs' boards

8. Flak lists installs:
     POST /graphql  (X-Space-ID: kanban-admin)
     Query: { _installs(spaceId: "kanban") { orgId } }
     → [ {orgId: "org-basecode"}, {orgId: "org-urbanway"} ]
```

## What NOT to use bundles for

- **Security boundary** — that's `publisher_org_id`. The bundle is just a grouping; two bundles under the same org are not isolated from each other.
- **Tenant grouping** — tenants never see bundles. They install individual spaces.
- **Versioning container** — each space version is independent today. Future work could extend bundles into version releases, but that is not implemented.
