# Construct Graph — Vision

## One Line
Firebase for Construct spaces — define models in your space, get a database and API automatically.

## Problem
Spaces currently store data in localStorage. No persistence, no multi-device sync, no shared data, no queries. The Company Manager space has employees in a single JSON blob. That doesn't scale.

## Solution
A backend service where spaces **define models locally** (in their space code), and when published, get:
- An isolated PostgreSQL schema per space per project
- Auto-generated GraphQL API for CRUD + filtering + relations
- Real-time subscriptions via WebSocket
- SDK composable (`useData()`) that replaces `useStorage()` for structured data
- Row-level security scoped to user/project/space

## "There's a space for that" — but now with data
```
Space defines model → Push/Publish → Database created → SDK works immediately
```

## Non-Goals (v1)
- Custom server-side logic (no cloud functions yet)
- File storage (use existing media tools)
- Authentication (use existing Construct auth)
- Analytics/metrics dashboard

## Example: Company Manager
```typescript
// space-company-manager/models/employee.ts
import { defineModel, field } from '@construct-space/data'

export const Employee = defineModel('employee', {
  name: field.string().required(),
  email: field.string().email(),
  department: field.string(),
  position: field.string(),
  salary: field.number(),
  startDate: field.date(),
  active: field.boolean().default(true),
})
```

```typescript
// In the space component
import { useData } from '@construct-space/data'
import { Employee } from '../models/employee'

const employees = useData(Employee)

// Create
await employees.create({ name: 'Alice', department: 'Engineering' })

// Query
const engineers = await employees.find({ department: 'Engineering' })

// Real-time
employees.subscribe((changes) => { /* reactive updates */ })
```
