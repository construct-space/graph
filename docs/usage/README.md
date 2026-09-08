# Graph Usage Guide

Construct Graph is a dynamic database + GraphQL API. Spaces define data models, Graph creates tables and serves a GraphQL API automatically.

## Table of Contents

- [Models](./models.md) — Defining data models with the SDK
- [Queries](./queries.md) — Reading data via GraphQL
- [Mutations](./mutations.md) — Creating, updating, deleting records
- [Relations](./relations.md) — belongsTo and hasMany with JOINs
- [Access Control](./access-control.md) — Per-model operation permissions
- [Schema Registration](./schema-registration.md) — How schemas get provisioned
- [Space Bundles](./bundles.md) — Publisher grouping, cross-space imports, distribution, installs, `publisher_admin`
- [CLI](./cli.md) — Graph CLI commands

## Quick Start

### 1. Add Graph to your space

```bash
bun add @construct-space/graph
```

### 2. Define a model

```typescript
// models/Task.ts
import { defineModel, field } from '@construct-space/graph'

export const Task = defineModel('task', {
  title: field.string().required(),
  done: field.boolean().default(false),
  priority: field.enum(['low', 'medium', 'high']),
})
```

### 3. Use it in a component

```vue
<script setup>
import { useGraph } from '@construct-space/graph'
import { Task } from '../models/Task'

const tasks = useGraph(Task)
const all = await tasks.find()
</script>

<template>
  <div v-for="t in all" :key="t.id">
    {{ t.title }} — {{ t.done ? 'Done' : 'Pending' }}
  </div>
</template>
```

### 4. Publish

```bash
construct publish
```

Graph automatically creates the database schema and tables when the space is published.
