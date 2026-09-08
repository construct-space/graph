# Construct Graph — Database Design

## PostgreSQL Schema Strategy

### Multi-Tenant Isolation via Schemas

Each space + project combination gets its own PostgreSQL schema:

```
construct_data (database)
├── _system (schema)                              # Platform metadata
│   ├── spaces                                    # Registered spaces
│   ├── manifests                                 # Model manifests per space version
│   ├── provisions                                # Which projects have which spaces provisioned
│   └── migrations                                # Global migration log
│
├── s_company_manager_p_abc123 (schema)           # Space: company-manager, Project: abc123
│   ├── employee                                  # Auto-generated from model
│   ├── department
│   ├── leave_request
│   └── _meta                                     # Schema-level metadata
│       ├── _migrations                           # Migration history for this schema
│       └── _changelog                            # Change audit log
│
├── s_kanban_p_abc123 (schema)                    # Space: kanban, Project: abc123
│   ├── board
│   ├── task
│   └── _meta/
│
└── s_company_manager_p_def456 (schema)           # Same space, different project
    ├── employee                                  # Completely isolated data
    ├── department
    └── ...
```

### Why PostgreSQL Schemas (Not Databases or Row-Level)

| Approach | Pros | Cons |
|----------|------|------|
| **Separate databases** | Total isolation | Connection overhead, hard to manage |
| **Row-level (tenant_id)** | Simple, one schema | Query complexity, risk of data leaks |
| **PostgreSQL schemas** | Strong isolation, shared connection pool, easy provisioning | Schema count limit (~10K practical) |

**Decision: Schemas.** Perfect for our scale. Each `CREATE SCHEMA` is instant, isolation is enforced at DB level, and we use a single connection pool.

### System Tables

```sql
-- _system.spaces
CREATE TABLE _system.spaces (
  id TEXT PRIMARY KEY,                    -- e.g., 'company-manager'
  name TEXT NOT NULL,                     -- e.g., 'Company Manager'
  latest_version TEXT NOT NULL,           -- e.g., '0.1.0'
  registered_at TIMESTAMPTZ DEFAULT now()
);

-- _system.manifests
CREATE TABLE _system.manifests (
  id SERIAL PRIMARY KEY,
  space_id TEXT REFERENCES _system.spaces(id),
  version TEXT NOT NULL,
  manifest JSONB NOT NULL,               -- Full data.manifest.json
  created_at TIMESTAMPTZ DEFAULT now(),
  UNIQUE(space_id, version)
);

-- _system.provisions
CREATE TABLE _system.provisions (
  id SERIAL PRIMARY KEY,
  space_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  user_id UUID NOT NULL,
  schema_name TEXT NOT NULL,             -- e.g., 's_company_manager_p_abc123'
  manifest_version TEXT NOT NULL,
  provisioned_at TIMESTAMPTZ DEFAULT now(),
  UNIQUE(space_id, project_id)
);
```

### Auto-Generated Tables

From this model definition:
```typescript
const Employee = defineModel('employee', {
  name: field.string().required(),
  email: field.string().email().unique(),
  salary: field.number(),
  active: field.boolean().default(true),
  department: relation.belongsTo(Department),
})
```

Generates:
```sql
CREATE TABLE s_company_manager_p_abc123.employee (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name TEXT NOT NULL,
  email TEXT UNIQUE,
  salary NUMERIC,
  active BOOLEAN DEFAULT true,
  department_id UUID REFERENCES s_company_manager_p_abc123.department(id),
  created_at TIMESTAMPTZ DEFAULT now(),
  updated_at TIMESTAMPTZ DEFAULT now(),
  created_by UUID
);

CREATE INDEX idx_employee_department_id ON s_company_manager_p_abc123.employee(department_id);
CREATE INDEX idx_employee_created_at ON s_company_manager_p_abc123.employee(created_at);
```

### Change Tracking

```sql
-- Per-schema changelog
CREATE TABLE s_company_manager_p_abc123._changelog (
  id BIGSERIAL PRIMARY KEY,
  table_name TEXT NOT NULL,
  record_id UUID NOT NULL,
  action TEXT NOT NULL,                  -- 'INSERT', 'UPDATE', 'DELETE'
  changes JSONB,                         -- { field: { old: x, new: y } }
  user_id UUID,
  created_at TIMESTAMPTZ DEFAULT now()
);
```

Populated via PostgreSQL trigger:
```sql
CREATE OR REPLACE FUNCTION changelog_trigger() RETURNS trigger AS $$
BEGIN
  INSERT INTO _changelog (table_name, record_id, action, changes, user_id)
  VALUES (
    TG_TABLE_NAME,
    COALESCE(NEW.id, OLD.id),
    TG_OP,
    CASE TG_OP
      WHEN 'UPDATE' THEN jsonb_build_object('old', to_jsonb(OLD), 'new', to_jsonb(NEW))
      WHEN 'DELETE' THEN to_jsonb(OLD)
      ELSE to_jsonb(NEW)
    END,
    current_setting('app.user_id', true)::UUID
  );
  IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
  NEW.updated_at = now();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
```

### Real-time via NOTIFY

On every insert/update/delete, trigger also sends:
```sql
PERFORM pg_notify(
  'data_change',
  json_build_object(
    'schema', TG_TABLE_SCHEMA,
    'table', TG_TABLE_NAME,
    'action', TG_OP,
    'id', COALESCE(NEW.id, OLD.id)
  )::text
);
```

The Go service LISTENs on `data_change` and fans out to WebSocket subscribers.

### Capacity Planning

| Metric | Limit |
|--------|-------|
| Schemas per database | ~10,000 practical |
| Tables per schema | Unlimited |
| Rows per table | Billions (with indexes) |
| Concurrent connections | 100 (pgxpool) |
| Schema creation time | < 10ms |
| Table creation time | < 5ms |

For v1, a single PostgreSQL instance handles everything. Shard by database when exceeding 10K active spaces.
