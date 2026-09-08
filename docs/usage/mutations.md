# Mutations

Create, update, and delete records.

## Using the SDK

```typescript
import { useGraph } from '@construct-space/graph'
import { Task } from '../models/Task'

const tasks = useGraph(Task)
```

### create

```typescript
const task = await tasks.create({
  title: 'Review PR',
  priority: 'high',
  done: false,
})
// Returns the created record with id, created_at, etc.
```

The `created_by` field is set automatically from the auth token.

### update

```typescript
const updated = await tasks.update('uuid-here', {
  done: true,
})
// Returns the updated record, updated_at is set automatically
```

### remove

```typescript
const deleted = await tasks.remove('uuid-here')
// Returns true on success
```

## Raw GraphQL

### Create

```graphql
mutation($input: JSON!) {
  createTask(input: $input) {
    id
    title
    done
    created_at
  }
}
```

Variables:
```json
{
  "input": {
    "title": "Review PR",
    "priority": "high"
  }
}
```

### Update

```graphql
mutation($id: ID!, $input: JSON!) {
  updateTask(id: $id, input: $input) {
    id
    title
    done
    updated_at
  }
}
```

Variables:
```json
{
  "id": "uuid-here",
  "input": { "done": true }
}
```

### Delete

```graphql
mutation($id: ID!) {
  deleteTask(id: $id)
}
```

Variables:
```json
{
  "id": "uuid-here"
}
```

## Mutation Naming Convention

| Operation | GraphQL Name | Example |
|-----------|-------------|---------|
| Create | `create{Model}` | `createTask`, `createEmployee` |
| Update | `update{Model}` | `updateTask`, `updateEmployee` |
| Delete | `delete{Model}` | `deleteTask`, `deleteEmployee` |

The model name is capitalized: `task` becomes `createTask`.

## Owner Verification

When a model has `update: 'owner'` or `delete: 'owner'` access:

- The server checks `created_by` matches the authenticated user
- Returns `"forbidden: you can only update your own records"` if not the owner

## Relation Fields in Mutations

For `belongsTo` relations, pass the FK column directly:

```typescript
// If Employee has: department: relation.belongsTo(Department)
await employees.create({
  first_name: 'Jane',
  email: 'jane@co.com',
  department_id: 'dept-uuid-here',  // note: _id suffix
})
```

In GraphQL:
```graphql
mutation($input: JSON!) {
  createEmployee(input: $input) {
    id
    first_name
    department_id
  }
}
```

Variables:
```json
{
  "input": {
    "first_name": "Jane",
    "department_id": "dept-uuid-here"
  }
}
```
