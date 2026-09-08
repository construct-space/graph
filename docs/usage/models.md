# Models

Models define the shape of your data. Each model becomes a database table with typed columns, constraints, and indexes.

## Defining a Model

```typescript
import { defineModel, field } from '@construct-space/graph'

export const Employee = defineModel('employee', {
  first_name: field.string().required(),
  last_name: field.string().required(),
  email: field.string().email().unique(),
  phone: field.string(),
  salary: field.number(),
  active: field.boolean().default(true),
  started_at: field.date(),
  metadata: field.json(),
  role: field.enum(['engineer', 'designer', 'manager']),
})
```

## Field Types

| Type | Builder | Postgres | SQLite |
|------|---------|----------|--------|
| String | `field.string()` | TEXT | TEXT |
| Integer | `field.int()` | INTEGER | INTEGER |
| Number | `field.number()` | NUMERIC | REAL |
| Boolean | `field.boolean()` | BOOLEAN | INTEGER |
| Date | `field.date()` | TIMESTAMPTZ | DATETIME |
| JSON | `field.json()` | JSONB | TEXT |
| Enum | `field.enum([...])` | TEXT + CHECK | TEXT + CHECK |

## Modifiers

Chain modifiers to add constraints and validation:

```typescript
field.string().required()         // NOT NULL
field.string().unique()           // UNIQUE constraint
field.string().index()            // database index for faster queries
field.string().default('hello')   // DEFAULT value
field.boolean().default(false)    // DEFAULT false
field.int().default(0)            // DEFAULT 0

// Validation (checked at SDK level)
field.string().email()            // validates email format
field.string().url()              // validates URL format
field.int().min(0)                // minimum value
field.int().max(100)              // maximum value
```

## Enum Fields

Enums restrict values to a predefined set. In the database, this becomes a `TEXT` column with a `CHECK` constraint.

```typescript
field.enum(['draft', 'published', 'archived'])
```

Generated SQL:
```sql
status TEXT CHECK (status IN ('draft','published','archived'))
```

## Auto-Generated Fields

Every table automatically includes these columns — you do not define them:

| Field | Type | Description |
|-------|------|-------------|
| `id` | UUID | Primary key, auto-generated |
| `created_at` | Timestamp | Set on insert |
| `updated_at` | Timestamp | Set on update |
| `created_by` | UUID | User ID from auth token |

## Model Options

### Scope

Controls data isolation:

```typescript
// Project scope (default) — each project gets its own data
// Schema: s_{spaceId}_p_{projectId}
defineModel('task', { ... })

// Company scope — shared across projects in a company
// Schema: c_{companyId}_s_{spaceId}
defineModel('setting', { ... }, { scope: 'company' })
```

### Access Control

See [Access Control](./access-control.md) for details.

```typescript
defineModel('task', {
  title: field.string().required(),
}, {
  access: {
    read: 'owner',
    create: 'authenticated',
    update: 'owner',
    delete: 'owner',
  }
})
```

## Naming Conventions

- Model names: lowercase, alphanumeric + underscores, start with a letter (`task`, `leave_request`)
- Field names: same rules (`first_name`, `email`, `status`)
- The SDK validates names and throws on invalid input

## Manifest Output

Models are compiled to `data.manifest.json` when publishing:

```json
{
  "version": 1,
  "models": [
    {
      "name": "employee",
      "fields": [
        { "name": "first_name", "type": "string", "required": true },
        { "name": "email", "type": "string", "unique": true, "validation": "email" },
        { "name": "active", "type": "boolean", "default": true },
        { "name": "role", "type": "enum", "values": ["engineer", "designer", "manager"] }
      ],
      "options": {
        "access": { "read": "member", "create": "admin", "update": "admin", "delete": "admin" }
      }
    }
  ]
}
```

## Model Registry

All models created with `defineModel` are tracked in an internal registry. The CLI uses `getRegisteredModels()` to extract the manifest during build/publish.

```typescript
import { getRegisteredModels, clearRegistry } from '@construct-space/graph'

const models = getRegisteredModels() // returns all defined models
clearRegistry()                      // useful for testing / hot-reload
```
