# Construct Graph — Implementation Plan

## Phase 1: Foundation (Week 1-2)

### 1.1 Data API Service (Go)
```
infra/graph/
├── cmd/data-api/main.go       # Entry point
├── internal/
│   ├── schema/                # Schema registry + migration engine
│   │   ├── registry.go        # Model registry (from data.manifest.json)
│   │   ├── migrator.go        # DDL generation + migration tracking
│   │   └── types.go           # FieldType, ModelDef, etc.
│   ├── engine/                # Query engine
│   │   ├── query.go           # WHERE builder, filters → SQL
│   │   ├── mutation.go        # INSERT/UPDATE/DELETE builders
│   │   └── resolver.go        # GraphQL resolver dispatch
│   ├── graphql/               # GraphQL layer
│   │   ├── handler.go         # HTTP handler (gqlgen)
│   │   ├── generator.go       # Model → GraphQL schema generator
│   │   └── subscriptions.go   # WebSocket subscriptions
│   ├── auth/                  # Auth middleware
│   │   └── jwt.go             # Validate Construct JWT tokens
│   └── realtime/              # Change broadcasting
│       └── pubsub.go          # PostgreSQL LISTEN/NOTIFY → WebSocket
├── Dockerfile
├── captain-definition
├── go.mod
└── go.sum
```

**Tasks:**
- [ ] Go project scaffold with pgx, gqlgen
- [ ] JWT validation (reuse Construct auth tokens)
- [ ] Schema registry: parse `data.manifest.json` → create PostgreSQL schemas/tables
- [ ] Basic CRUD resolver: create, findOne, findMany, update, delete
- [ ] WHERE clause builder with filter operators ($gt, $in, $like, etc.)
- [ ] Deploy to CapRover as `data-api` app

### 1.2 Model Definition Package (TypeScript)
```
construct-data/
├── src/
│   ├── define.ts              # defineModel(), field, relation
│   ├── types.ts               # Model types, FieldDef, etc.
│   ├── client.ts              # GraphQL client
│   ├── composable.ts          # useData() Vue composable
│   ├── manifest.ts            # Extract models → data.manifest.json
│   └── index.ts               # Exports
├── package.json               # @construct-space/data
└── tsconfig.json
```

**Tasks:**
- [ ] `defineModel()` and `field.*` API
- [ ] `useData()` composable with create/find/update/delete
- [ ] GraphQL query/mutation generation from model definitions
- [ ] Manifest extractor (used by `construct publish`)
- [ ] Publish as `@construct-space/data` on npm

### 1.3 CLI Integration
- [ ] `construct publish` extracts `data.manifest.json` from space source
- [ ] Registry API endpoint to receive manifest + provision schema
- [ ] Rollback on failed migration

---

## Phase 2: Queries & Relations (Week 3-4)

### 2.1 Advanced Queries
- [ ] Sorting (orderBy with multiple fields)
- [ ] Pagination (limit/offset + cursor-based)
- [ ] Count queries
- [ ] Aggregations (sum, avg, min, max — v2)
- [ ] Full-text search on string fields

### 2.2 Relations
- [ ] belongsTo → foreign key + JOIN resolver
- [ ] hasMany → reverse lookup resolver
- [ ] Nested includes (`include: ['department', 'department.manager']`)
- [ ] Filter by relation fields (`department.code: 'ENG'`)

### 2.3 Validation
- [ ] Server-side field validation (required, unique, email, min/max)
- [ ] Unique constraint enforcement
- [ ] Enum value validation
- [ ] Type coercion (string dates → timestamptz)

---

## Phase 3: Real-time & Sync (Week 5-6)

### 3.1 Subscriptions
- [ ] PostgreSQL LISTEN/NOTIFY on data changes
- [ ] WebSocket subscription endpoint
- [ ] Client-side auto-update of reactive queries
- [ ] Per-space, per-model subscription channels

### 3.2 Change Tracking
- [ ] `_changelog` table per schema (who changed what, when)
- [ ] `previousRecord` in subscription events
- [ ] Soft-delete support (mark deleted, filter by default)

### 3.3 Optimistic Updates
- [ ] SDK applies changes locally before server confirms
- [ ] Rollback on server error
- [ ] Conflict resolution (last-write-wins for v1)

---

## Phase 4: Production Hardening (Week 7-8)

### 4.1 Security
- [ ] Row-level security: users only see their project's data
- [ ] Rate limiting per user/space
- [ ] Input sanitization (SQL injection prevention via parameterized queries)
- [ ] CORS configuration for Construct domains

### 4.2 Performance
- [ ] Connection pooling (pgxpool)
- [ ] Query result caching (Redis or in-memory)
- [ ] Batch inserts/updates
- [ ] Index auto-creation for filtered/sorted fields

### 4.3 Operations
- [ ] Health check endpoint
- [ ] Migration status endpoint
- [ ] Schema introspection endpoint (for debugging)
- [ ] Logging and error tracking

---

## Deployment

### CapRover App: `data-api`
```
captain-definition:
  schemaVersion: 2
  dockerfilePath: ./Dockerfile
```

### Environment Variables
```env
DATABASE_URL=postgres://user:pass@host:5432/construct_data
JWT_SECRET=<same as construct-api>
CORS_ORIGINS=https://lisaos.dev,tauri://localhost
PORT=4000
```

### PostgreSQL
- Use existing CapRover PostgreSQL or provision dedicated instance
- Each space+project combo gets its own schema
- Shared `_system` schema for metadata, manifests, migrations

---

## Migration Path for Existing Spaces

### Company Manager Example
1. Add `@construct-space/data` dependency
2. Create `models/employee.ts` with `defineModel()`
3. Replace `useStorage()` calls with `useData(Employee)`
4. Run `construct publish` — manifest extracted, schema provisioned
5. Data migrated from localStorage on first load (one-time)

### Backward Compatibility
- `useStorage()` continues to work (local-only)
- `useData()` is opt-in per space
- Spaces without models are unaffected
- No breaking changes to existing SDK

---

## Success Metrics
- Space developer defines a model and has a working API in < 5 minutes
- CRUD operations complete in < 50ms (p99)
- Real-time updates arrive in < 200ms
- Zero-config for space developers (auth, URLs, schemas all automatic)
- Company Manager migrated from localStorage to Graph as proof of concept
