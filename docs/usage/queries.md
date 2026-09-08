# Queries

Read data from Graph using the SDK composable or raw GraphQL.

## Using the SDK

```typescript
import { useGraph } from '@construct-space/graph'
import { Task } from '../models/Task'

const tasks = useGraph(Task)
```

### find — List records

```typescript
// All records (default limit: 100)
const all = await tasks.find()

// With filters
const urgent = await tasks.find({
  where: { priority: 'high', done: false },
  orderBy: { created_at: 'desc' },
  limit: 20,
  offset: 0,
})
```

### findOne — Single record by ID

```typescript
const task = await tasks.findOne('uuid-here')
// returns null if not found
```

### count — Record count

```typescript
const total = await tasks.count()
const active = await tasks.count({ where: { done: false } })
```

### include — With related data

```typescript
const employees = await empClient.find({
  include: ['department'],  // includes department: { id: '...' }
})
```

For full nested data, use raw GraphQL (see below).

## Filter Operators

Pass operators as objects in the `where` clause:

```typescript
await tasks.find({
  where: {
    // Exact match
    status: 'active',

    // Comparison
    priority: { $gt: 3 },
    created_at: { $gte: '2026-01-01' },
    score: { $lt: 100 },
    age: { $lte: 65 },

    // Not equal
    role: { $ne: 'guest' },

    // Pattern matching
    email: { $like: '%@company.com' },

    // In set
    status: { $in: ['active', 'pending'] },

    // Null check
    deleted_at: { $null: true },    // IS NULL
    email: { $null: false },        // IS NOT NULL
  }
})
```

| Operator | SQL | Example |
|----------|-----|---------|
| `$gt` | `>` | `{ age: { $gt: 25 } }` |
| `$gte` | `>=` | `{ score: { $gte: 80 } }` |
| `$lt` | `<` | `{ price: { $lt: 100 } }` |
| `$lte` | `<=` | `{ quantity: { $lte: 0 } }` |
| `$ne` | `!=` | `{ status: { $ne: 'archived' } }` |
| `$like` | `LIKE` | `{ name: { $like: 'J%' } }` |
| `$in` | `IN` | `{ role: { $in: ['admin', 'mod'] } }` |
| `$null` | `IS NULL` | `{ deleted_at: { $null: true } }` |

## Ordering

```typescript
await tasks.find({
  orderBy: { created_at: 'desc' }
})

await tasks.find({
  orderBy: { priority: 'asc' }
})
```

Default ordering is `created_at DESC` when no `orderBy` is specified.

## Pagination

```typescript
// Page 1
const page1 = await tasks.find({ limit: 20, offset: 0 })

// Page 2
const page2 = await tasks.find({ limit: 20, offset: 20 })
```

Default limit is 100 if not specified.

## Raw GraphQL

For queries the SDK doesn't cover (nested relations, custom shapes):

```typescript
const data = await tasks.query(`{
  tasks(limit: 10) {
    id
    title
    done
    created_at
  }
}`)
// data.tasks = [{ id, title, done, created_at }, ...]
```

### With variables

```typescript
const data = await tasks.query(
  `query($where: JSON) {
    tasks(where: $where) {
      id title
    }
  }`,
  { where: { done: false } }
)
```

### Nested relations

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

See [Relations](./relations.md) for details on how relations are resolved.

## Required Headers

The SDK sets these automatically. For raw HTTP requests:

| Header | Required | Description |
|--------|----------|-------------|
| `X-Space-ID` | Yes | Space identifier |
| `X-Project-ID` | No | Defaults to "default" |
| `X-Company-ID` | For company scope | Company identifier |
| `Authorization` | For auth'd operations | `Bearer {token}` |
