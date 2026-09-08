# Construct Graph — SDK Design

## Package: `@construct-space/data`

Published alongside `@construct-space/sdk`. Provides:
- Model definition helpers (`defineModel`, `field`, `relation`)
- Data access composable (`useData`)
- GraphQL client (auto-configured)
- Real-time subscriptions
- Offline cache with optimistic updates

## Installation

```bash
bun add @construct-space/data
```

## Core API: `useData(Model)`

```typescript
import { useData } from '@construct-space/data'
import { Employee } from '../models/employee'

const {
  // Reactive state
  items,          // Ref<Employee[]> — current query results
  loading,        // Ref<boolean>
  error,          // Ref<string | null>
  count,          // Ref<number> — total matching count

  // CRUD
  create,         // (input) => Promise<Employee>
  update,         // (id, input) => Promise<Employee>
  remove,         // (id) => Promise<boolean>
  upsert,         // (input) => Promise<Employee>

  // Queries
  find,           // (where?, options?) => Promise<Employee[]>
  findOne,        // (id) => Promise<Employee | null>
  findMany,       // (where?, options?) => Promise<Employee[]>

  // Real-time
  subscribe,      // () => void — start listening for changes
  unsubscribe,    // () => void — stop listening

  // Pagination
  page,           // Ref<number>
  pageSize,       // Ref<number>
  hasMore,        // Ref<boolean>
  loadMore,       // () => Promise<void>
} = useData(Employee)
```

## Query API

### Basic Queries
```typescript
// All employees
const all = await employees.find()

// With filter
const engineers = await employees.find({
  department: 'Engineering',
  active: true,
})

// Complex filters
const senior = await employees.find({
  salary: { $gte: 100000 },
  startDate: { $lt: new Date('2024-01-01') },
  status: { $in: ['active', 'onLeave'] },
})
```

### Filter Operators
```typescript
{ field: value }              // equals
{ field: { $ne: value } }     // not equals
{ field: { $gt: value } }     // greater than
{ field: { $gte: value } }    // greater or equal
{ field: { $lt: value } }     // less than
{ field: { $lte: value } }    // less or equal
{ field: { $in: [...] } }     // in array
{ field: { $nin: [...] } }    // not in array
{ field: { $like: '%pattern%' } }  // SQL LIKE
{ field: { $null: true } }    // IS NULL
```

### Sorting & Pagination
```typescript
const result = await employees.find(
  { active: true },
  {
    orderBy: { salary: 'desc', name: 'asc' },
    limit: 20,
    offset: 0,
  }
)
```

### Relations
```typescript
// Eager load relations
const result = await employees.find(
  {},
  { include: ['department'] }
)
// result[0].department.name === 'Engineering'

// Filter by relation
const result = await employees.find({
  'department.code': 'ENG',
})
```

## Reactive Queries

```typescript
// Auto-fetches and stays reactive
const { items: employees } = useData(Employee, {
  where: { active: true },
  orderBy: { name: 'asc' },
  realtime: true, // auto-subscribe to changes
})

// In template:
// <div v-for="emp in employees" :key="emp.id">{{ emp.name }}</div>
```

## CRUD Operations

```typescript
// Create
const alice = await employees.create({
  name: 'Alice',
  email: 'alice@company.com',
  department: deptId,
  salary: 95000,
})

// Update
await employees.update(alice.id, {
  salary: 105000,
  status: 'active',
})

// Delete
await employees.remove(alice.id)

// Upsert (create or update by unique field)
await employees.upsert({
  email: 'alice@company.com',  // unique field
  name: 'Alice Updated',
  salary: 110000,
})
```

## Real-time Subscriptions

```typescript
// Subscribe to changes
employees.subscribe()

// The `items` ref auto-updates when:
// - Another user creates/updates/deletes a record
// - Another device modifies data
// - Another space writes to the same model

// Manual callback
employees.onChanged((event) => {
  // event.type: 'created' | 'updated' | 'deleted'
  // event.record: the changed record
  // event.previousRecord: (on update) the old values
})

// Cleanup
onUnmounted(() => employees.unsubscribe())
```

## Offline Support (v2)

```typescript
const { items, pendingChanges } = useData(Employee, {
  offline: true, // cache locally, sync when online
})

// Works offline:
await employees.create({ name: 'Bob' }) // queued locally
// Syncs automatically when connection restored
```

## Configuration

SDK auto-configures from Construct context (no manual setup needed):
```typescript
// Internally resolves:
// - API URL from Construct app config
// - Auth token from Construct auth store
// - Space ID from current space context
// - Project ID from current project context
```

For development/testing:
```typescript
import { configureData } from '@construct-space/data'

configureData({
  apiUrl: 'http://localhost:4000/data',
  token: 'dev-token',
  spaceId: 'company-manager',
  projectId: 'test-project',
})
```
