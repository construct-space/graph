# Graph Realtime Events Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add realtime change delivery to Construct Graph so chat-like spaces can persist messages through GraphQL and receive live create/update/delete events.

**Architecture:** Graph remains the write source: clients still use GraphQL mutations for durable writes. The engine writes each mutation to a durable `_system.graph_events` outbox, sends a local process notification for low-latency streaming, and the realtime endpoint replays missed events by cursor before following live updates. The SDK uses authenticated fetch streaming instead of browser `WebSocket` so it can send the same bearer/API-key headers as GraphQL without putting tokens in URLs.

**Tech Stack:** Go HTTP/SSE, GORM over PostgreSQL/SQLite, Graph engine mutation hooks, TypeScript fetch streaming.

---

## File Structure

- Modify `infra/graph/internal/schema/registry.go`
  - Create `_system.graph_events` in PostgreSQL and SQLite system schemas.
- Modify `infra/graph/internal/engine/engine.go`
  - Add `GraphEvent`, event sink hook, outbox insert, `pg_notify` for PostgreSQL, and event replay queries.
  - Emit `created`, `updated`, and `deleted` events from `Create`, `Update`, and `Delete`.
- Create `infra/graph/internal/graphql/realtime.go`
  - Authenticated realtime SSE handler.
  - Request context/schema resolution matching GraphQL.
  - Read access enforcement and owner filters.
  - Cursor replay and live follow loop.
- Modify `infra/graph/cmd/graph/main.go`
  - Instantiate the realtime hub.
  - Attach it to the engine.
  - Route `GET /realtime/stream` and preflight.
- Modify `packages/graph-sdk/src/composable.ts`
  - Add realtime event/subscription types and `subscribe()` to `GraphClient`.
  - Implement authenticated fetch stream parsing with reconnect and cursor resume.
- Add tests:
  - `infra/graph/internal/engine/realtime_test.go`
  - `infra/graph/internal/graphql/realtime_test.go`

---

### Task 1: Durable Event Outbox

**Files:**
- Modify: `infra/graph/internal/schema/registry.go`
- Modify: `infra/graph/internal/engine/engine.go`
- Test: `infra/graph/internal/engine/realtime_test.go`

- [x] **Step 1: Write failing engine test**

Test that `Create`, `Update`, and `Delete` append graph events with action, schema, table, record id, payload, and previous payload for updates/deletes.

- [x] **Step 2: Run test and verify RED**

Run: `go test ./internal/engine -run TestGraphEvents -v`

- [x] **Step 3: Implement outbox table and engine event emission**

Create `_system.graph_events` during system init. Add engine event writing after successful mutations.

- [x] **Step 4: Verify GREEN**

Run: `go test ./internal/engine -run TestGraphEvents -v`

---

### Task 2: Realtime SSE Endpoint

**Files:**
- Create: `infra/graph/internal/graphql/realtime.go`
- Modify: `infra/graph/cmd/graph/main.go`
- Test: `infra/graph/internal/graphql/realtime_test.go`

- [x] **Step 1: Write failing handler test**

Test that `GET /realtime/stream?model=message&cursor=0` emits a matching stored event and advances by event id.

- [x] **Step 2: Run test and verify RED**

Run: `go test ./internal/graphql -run TestRealtime -v`

- [x] **Step 3: Implement hub + handler**

Add local process fanout, cursor replay from `_system.graph_events`, simple `where` matching, owner filter, and heartbeat.

- [x] **Step 4: Verify GREEN**

Run: `go test ./internal/graphql -run TestRealtime -v`

---

### Task 3: SDK Subscribe API

**Files:**
- Modify: `packages/graph-sdk/src/types.ts`
- Modify: `packages/graph-sdk/src/composable.ts`

- [x] **Step 1: Add SDK types and client method**

Expose:

```ts
const stop = messages.subscribe({
  where: { room_id: roomId },
  onEvent(event) {
    if (event.action === 'created') append(event.record)
  },
})
```

- [x] **Step 2: Implement fetch-stream SSE parser**

Use `getHeaders()` for auth, reconnect with last event id, and return an unsubscribe function.

- [x] **Step 3: Verify TypeScript build**

Run: `bun run build` from `packages/graph-sdk`.

---

### Task 4: Full Verification

- [x] Run `go test ./...` from `infra/graph`.
- [x] Run `bun run build` from `packages/graph-sdk`.
- [ ] Run `git diff --check` from `/Users/flakerim/Construct`.
