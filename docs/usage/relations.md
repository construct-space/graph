# Relations

Graph supports `belongsTo` and `hasMany` relations between models. Relations are defined in the space manifest and resolved automatically in GraphQL queries without N+1.

## Defining Relations

### belongsTo

Creates a foreign key column on the child table.

```typescript
import { defineModel, field, relation } from '@construct-space/graph'

const Card = defineModel('card', {
  title: field.string().required(),
  color: field.string(),
})

const Element = defineModel('element', {
  card: relation.belongsTo(Card, { onDelete: 'cascade' }),
  component: field.string().required(),
  props: field.json(),
})
```

This creates `element.card_id` (UUID) referencing `card(id)` with `ON DELETE CASCADE`.

**Options:**

| Option | Values | Default | Description |
|--------|--------|---------|-------------|
| `onDelete` | `cascade`, `set_null`, `restrict` | `set_null` | What happens when parent is deleted |
| `nullable` | `true`, `false` | `false` | Whether the FK can be null |

### hasMany

Virtual inverse — no column created. Declares that a model has many related records in another table.

```typescript
const Card = defineModel('card', {
  title: field.string().required(),
  elements: relation.hasMany(Element),
})
```

## Querying Relations

### belongsTo (nested parent)

Resolved via SQL `LEFT JOIN` — single query.

```graphql
{
  elements(limit: 20) {
    id
    component
    card {
      id
      title
      color
    }
  }
}
```

Generated SQL:
```sql
SELECT t.*, row_to_json(j0.*) AS "card"
FROM s_canvas_p_default.element t
LEFT JOIN s_canvas_p_default.card j0 ON j0.id = t.card_id
ORDER BY t.created_at DESC
LIMIT 20
```

### hasMany (nested children)

Resolved via batch `IN` query — exactly 2 queries total.

```graphql
{
  cards(limit: 10) {
    id
    title
    elements {
      id
      component
      props
    }
  }
}
```

Generated SQL:
```sql
-- Query 1: fetch cards
SELECT * FROM s_canvas_p_default.card ORDER BY created_at DESC LIMIT 10

-- Query 2: batch fetch all elements for those cards
SELECT * FROM s_canvas_p_default.element
WHERE card_id IN ('id1', 'id2', 'id3', ...)
ORDER BY created_at DESC
```

### Single record with relations

```graphql
{
  card(id: "uuid-here") {
    id
    title
    elements {
      id
      component
    }
  }
}
```

## Manifest Format

Relations in `data.manifest.json`:

```json
{
  "version": 1,
  "models": [
    {
      "name": "card",
      "fields": [
        { "name": "title", "type": "string", "required": true },
        { "name": "color", "type": "string" },
        { "name": "elements", "type": "relation", "relation": "hasMany", "target": "element" }
      ]
    },
    {
      "name": "element",
      "fields": [
        { "name": "card", "type": "relation", "relation": "belongsTo", "target": "card", "on_delete": "cascade" },
        { "name": "component", "type": "string", "required": true },
        { "name": "props", "type": "json" }
      ]
    }
  ]
}
```

## How It Works

### Resolution Flow

1. GraphQL handler parses the query for nested field names (e.g. `card`, `elements`)
2. Registry looks up relation definitions for those fields
3. For **belongsTo**: engine builds a `LEFT JOIN` query with `row_to_json` to embed the parent as a nested JSON object
4. For **hasMany**: engine runs the main query first, collects all parent IDs, then runs a single `WHERE fk IN (...)` query and groups results by FK

### Key Files

| File | Role |
|------|------|
| `internal/schema/types.go` | `FieldDef` with Relation, Target, OnDelete fields |
| `internal/schema/registry.go` | `GetRelationFields()` — discovers relations for a model |
| `internal/engine/engine.go` | `FindManyWithJoins()`, `BatchLoadRelated()` |
| `internal/graphql/handler.go` | `buildRelationSpecs()`, `parseNestedFields()` — wires it all together |

### N+1 Prevention

| Relation | Strategy | Queries |
|----------|----------|---------|
| belongsTo | LEFT JOIN | 1 |
| hasMany | Batch IN | 2 |
| No relations requested | Plain SELECT | 1 |

Relations are only resolved when nested fields are requested in the GraphQL query. If you only query scalar fields, no JOINs are added.
