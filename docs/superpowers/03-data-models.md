# Construct Graph — Data Model Definition

## How Spaces Define Models

Models live in the space source code as TypeScript files. They are:
1. **Defined locally** — space developer writes them
2. **Extracted at publish** — `construct publish` reads model definitions
3. **Registered server-side** — Data API creates/migrates tables
4. **Available immediately** — SDK connects to the generated API

## Model Definition API

### `defineModel(name, fields)`

```typescript
import { defineModel, field, relation } from '@construct-space/data'

export const Department = defineModel('department', {
  name: field.string().required(),
  code: field.string().unique(),
  headCount: field.int().default(0),
})

export const Employee = defineModel('employee', {
  // Primitives
  name: field.string().required(),
  email: field.string().email().unique(),
  phone: field.string(),

  // Numbers
  salary: field.number(),
  age: field.int(),

  // Boolean
  active: field.boolean().default(true),

  // Dates
  startDate: field.date(),
  birthDate: field.date(),

  // Enum
  status: field.enum(['active', 'onLeave', 'terminated']).default('active'),

  // JSON (flexible/unstructured)
  metadata: field.json(),

  // Relations
  department: relation.belongsTo(Department),
})

export const LeaveRequest = defineModel('leave_request', {
  employee: relation.belongsTo(Employee),
  type: field.enum(['vacation', 'sick', 'personal']),
  startDate: field.date().required(),
  endDate: field.date().required(),
  status: field.enum(['pending', 'approved', 'rejected']).default('pending'),
  reason: field.string(),
  approvedBy: relation.belongsTo(Employee, { nullable: true }),
})
```

## Field Types

| Type | SQL | GraphQL | Description |
|------|-----|---------|-------------|
| `field.string()` | TEXT | String | Variable-length text |
| `field.int()` | INTEGER | Int | Integer number |
| `field.number()` | NUMERIC | Float | Decimal number |
| `field.boolean()` | BOOLEAN | Boolean | True/false |
| `field.date()` | TIMESTAMPTZ | DateTime | Date with timezone |
| `field.enum([...])` | TEXT + CHECK | Enum | Constrained string |
| `field.json()` | JSONB | JSON | Flexible JSON data |

## Field Modifiers

```typescript
field.string()              // nullable by default
  .required()               // NOT NULL
  .unique()                 // UNIQUE constraint
  .default('value')         // DEFAULT value
  .email()                  // validation: email format
  .min(1).max(100)          // validation: length/range
  .index()                  // CREATE INDEX
```

## Relations

```typescript
relation.belongsTo(Model)          // Foreign key on this table
relation.hasMany(Model)            // Virtual — resolved via FK on other table
relation.belongsTo(Model, {
  nullable: true,                  // Optional relation
  onDelete: 'cascade',            // CASCADE | SET NULL | RESTRICT
})
```

## Auto-Generated Fields

Every model automatically gets:
- `id: UUID` — primary key, auto-generated
- `createdAt: DateTime` — set on insert
- `updatedAt: DateTime` — set on update
- `createdBy: UUID` — from auth context (who created it)

## Model Manifest

When `construct publish` runs, it extracts model definitions into a `data.manifest.json`:

```json
{
  "version": 1,
  "models": [
    {
      "name": "employee",
      "fields": [
        { "name": "name", "type": "string", "required": true },
        { "name": "email", "type": "string", "unique": true, "validation": "email" },
        { "name": "salary", "type": "number" },
        { "name": "active", "type": "boolean", "default": true },
        { "name": "status", "type": "enum", "values": ["active", "onLeave", "terminated"], "default": "active" },
        { "name": "department", "type": "relation", "relation": "belongsTo", "target": "department" }
      ]
    },
    {
      "name": "department",
      "fields": [
        { "name": "name", "type": "string", "required": true },
        { "name": "code", "type": "string", "unique": true }
      ]
    }
  ]
}
```

## Schema Migrations

When a space is updated with changed models:

1. **Additive changes** (new fields, new models) — auto-applied
2. **Non-breaking changes** (add index, change default) — auto-applied
3. **Breaking changes** (remove field, change type) — requires confirmation
4. **Data preserved** — columns are never dropped without explicit flag

Migration log stored in `_migrations` table per schema:
```sql
CREATE TABLE space_company_manager_proj_123._migrations (
  id SERIAL PRIMARY KEY,
  version INT NOT NULL,
  applied_at TIMESTAMPTZ DEFAULT now(),
  changes JSONB NOT NULL
);
```
