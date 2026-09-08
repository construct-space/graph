# Access Control

Graph provides per-model, per-operation access control. Define who can read, create, update, and delete records.

## Defining Access Rules

```typescript
import { defineModel, field } from '@construct-space/graph'

const Task = defineModel('task', {
  title: field.string().required(),
  done: field.boolean().default(false),
}, {
  access: {
    read: 'owner',
    create: 'authenticated',
    update: 'owner',
    delete: 'admin',
  }
})
```

## Access Levels

| Level | Description | Auth Required |
|-------|-------------|---------------|
| `public` | Anyone, no authentication needed | No |
| `authenticated` | Any user with a valid token | Yes |
| `owner` | Only the record creator | Yes |
| `member` | Project or company member | Yes |
| `admin` | Admin role | Yes |
| `none` | Operation completely disabled | N/A |

## How Each Level Works

### public

No authentication required. Anyone can perform the operation.

```typescript
{ read: 'public' }  // anyone can read, even without logging in
```

### authenticated

Requires a valid auth token. Any logged-in user can perform the operation.

```typescript
{ create: 'authenticated' }  // any logged-in user can create
```

### owner

Requires authentication. For reads, automatically filters by `created_by = currentUser`. For updates/deletes, verifies the record's `created_by` matches the current user.

```typescript
{ read: 'owner', update: 'owner', delete: 'owner' }
// Users can only see/edit/delete their own records
```

### member

Requires authentication and a company/project context. For company-scoped spaces, the user must provide `X-Company-ID` header.

```typescript
{ read: 'member' }  // any project/company member can read
```

### admin

Requires authentication and admin role. Currently enforced at the authentication level.

```typescript
{ delete: 'admin' }  // only admins can delete
```

### none

Operation is completely disabled. Returns an error if attempted.

```typescript
{ delete: 'none' }  // nobody can delete records
```

## Default Behavior

If no access rules are defined for a model, all operations default to `authenticated` — any logged-in user can read, create, update, and delete.

## Common Patterns

### Personal data (notes, tasks)

```typescript
{
  access: {
    read: 'owner',
    create: 'authenticated',
    update: 'owner',
    delete: 'owner',
  }
}
```

### Shared team data (wiki, docs)

```typescript
{
  access: {
    read: 'member',
    create: 'authenticated',
    update: 'owner',
    delete: 'admin',
  }
}
```

### Reference data (categories, tags)

```typescript
{
  access: {
    read: 'member',
    create: 'admin',
    update: 'admin',
    delete: 'admin',
  }
}
```

### Public read, auth write

```typescript
{
  access: {
    read: 'public',
    create: 'authenticated',
    update: 'owner',
    delete: 'owner',
  }
}
```

## Owner Filter Implementation

When `read: 'owner'`, Graph automatically adds `WHERE created_by = ?` to all queries. You don't need to filter manually — the SDK and GraphQL layer handle this transparently.

```typescript
// With read: 'owner', this only returns the current user's tasks
const myTasks = await tasks.find()
```

For updates and deletes with `owner` access, Graph fetches the record first and compares `created_by` with the current user ID before allowing the operation.
