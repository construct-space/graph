# Construct Graph — Architecture

## System Overview

```
┌─────────────────────────────────────────────────────┐
│                   CONSTRUCT APP                      │
│                                                      │
│  ┌──────────┐  ┌──────────┐  ┌──────────────────┐  │
│  │  Space A  │  │  Space B  │  │  Company Manager  │  │
│  │           │  │           │  │                    │  │
│  │ useData() │  │ useData() │  │    useData()       │  │
│  └────┬──────┘  └────┬──────┘  └────────┬───────────┘  │
│       │              │                   │              │
│  ┌────┴──────────────┴───────────────────┴───────┐     │
│  │           @construct-space/data (SDK)          │     │
│  │  GraphQL client · Cache · Optimistic updates   │     │
│  └───────────────────────┬───────────────────────┘     │
└──────────────────────────┼──────────────────────────────┘
                           │ HTTPS / WSS
                           ▼
┌──────────────────────────────────────────────────────────┐
│                    CONSTRUCT DATA API                     │
│                     (Go service)                          │
│                                                           │
│  ┌─────────────┐  ┌──────────────┐  ┌────────────────┐  │
│  │  GraphQL     │  │  Schema      │  │  Realtime       │  │
│  │  Resolver    │  │  Registry    │  │  Subscriptions   │  │
│  └──────┬──────┘  └──────┬───────┘  └───────┬─────────┘  │
│         │                │                   │             │
│  ┌──────┴────────────────┴───────────────────┴─────────┐  │
│  │              Data Engine                              │  │
│  │  Dynamic table creation · Query builder · Migrations  │  │
│  └───────────────────────┬───────────────────────────────┘  │
│                          │                                   │
│  ┌───────────────────────┴───────────────────────────────┐  │
│  │                   PostgreSQL                            │  │
│  │  schema: space_{spaceId}_{projectId}                    │  │
│  │  table:  employee, department, leave_request ...        │  │
│  └─────────────────────────────────────────────────────────┘  │
└───────────────────────────────────────────────────────────────┘
```

## Technology Choices

### Database: PostgreSQL
**Why not NoSQL:**
- Spaces define models with fields and types — that's relational
- GraphQL maps cleanly to SQL with JOINs
- PostgreSQL schemas give free multi-tenant isolation
- JSONB columns for flexible/unstructured fields when needed
- Proven at scale, excellent tooling

**Schema isolation:**
```sql
CREATE SCHEMA space_company_manager_proj_123;
CREATE TABLE space_company_manager_proj_123.employee (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name TEXT NOT NULL,
  email TEXT,
  department TEXT,
  salary NUMERIC,
  active BOOLEAN DEFAULT true,
  created_at TIMESTAMPTZ DEFAULT now(),
  updated_at TIMESTAMPTZ DEFAULT now(),
  created_by UUID REFERENCES users(id)
);
```

### API: GraphQL (via gqlgen)
**Why GraphQL over REST:**
- Auto-generated from model definitions — no manual endpoint wiring
- Filtering, pagination, sorting built into query language
- Subscriptions for real-time (over WebSocket)
- Spaces get exactly the fields they need (no over-fetching)
- Single endpoint, typed responses

**Auto-generated schema from model:**
```graphql
type Employee {
  id: ID!
  name: String!
  email: String
  department: String
  salary: Float
  active: Boolean!
  createdAt: DateTime!
  updatedAt: DateTime!
}

type Query {
  employee(id: ID!): Employee
  employees(
    where: EmployeeWhere
    orderBy: EmployeeOrderBy
    limit: Int
    offset: Int
  ): [Employee!]!
  employeesCount(where: EmployeeWhere): Int!
}

type Mutation {
  createEmployee(input: CreateEmployeeInput!): Employee!
  updateEmployee(id: ID!, input: UpdateEmployeeInput!): Employee!
  deleteEmployee(id: ID!): Boolean!
  upsertEmployee(input: CreateEmployeeInput!): Employee!
}

type Subscription {
  employeeChanged: EmployeeChange!
}
```

### Backend: Go
- Matches existing construct-api stack
- gqlgen for type-safe GraphQL
- pgx for PostgreSQL
- Deployed alongside construct-api (or as separate service)

### SDK: TypeScript (@construct-space/data)
- Thin GraphQL client with Vue 3 reactivity
- Replaces `useStorage()` for structured data
- Offline-first with optimistic updates
- Published on npm alongside @construct-space/sdk

## Request Flow

```
1. Space calls: employees.create({ name: 'Alice' })
2. SDK sends GraphQL mutation with auth token + space ID
3. Data API:
   a. Validates auth (JWT from Construct auth)
   b. Resolves schema: space_{spaceId}_{projectId}
   c. Validates input against registered model
   d. Executes INSERT
   e. Broadcasts change via WebSocket subscription
   f. Returns created record
4. SDK updates local cache + reactive refs
5. Other connected clients receive subscription update
```

## Auth & Security

- **Authentication:** Reuse existing Construct JWT tokens
- **Authorization:** Row-level — users can only access their project's data
- **Isolation:** PostgreSQL schemas — complete data separation per space+project
- **Input validation:** Server-side against registered model schema
- **Rate limiting:** Per-user, per-space quotas
