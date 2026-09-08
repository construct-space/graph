package engine_test

import (
	"context"
	"testing"

	"construct-graph/internal/engine"
	"construct-graph/internal/schema"
)

func TestGraphEventsAreStoredForMutations(t *testing.T) {
	db, err := engine.Connect(":memory:")
	if err != nil {
		t.Fatalf("connect sqlite: %v", err)
	}
	reg := schema.NewRegistry(db)
	if err := reg.InitSystem(); err != nil {
		t.Fatalf("init system: %v", err)
	}
	if err := reg.Register("chat", "Chat", "0.0.1", schema.Manifest{
		Models: []schema.ModelDef{{
			Name: "message",
			Fields: []schema.FieldDef{
				{Name: "room_id", Type: "string", Index: true},
				{Name: "body", Type: "string"},
			},
		}},
	}, "default", "user-a"); err != nil {
		t.Fatalf("register: %v", err)
	}

	eng := engine.New(db)
	var notified []int64
	eng.SetEventSink(func(id int64) { notified = append(notified, id) })
	ctx := context.Background()
	schemaName := schema.SchemaName("chat", "default")

	created, err := eng.Create(ctx, schemaName, "message", map[string]any{
		"room_id": "general",
		"body":    "hello",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := created["id"].(string)

	updated, err := eng.Update(ctx, schemaName, "message", id, map[string]any{"body": "edited"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated["body"] != "edited" {
		t.Fatalf("expected updated body, got %#v", updated["body"])
	}

	if err := eng.Delete(ctx, schemaName, "message", id); err != nil {
		t.Fatalf("delete: %v", err)
	}

	events, err := eng.ListGraphEvents(ctx, schemaName, "message", 0, 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d: %#v", len(events), events)
	}
	if len(notified) != 3 {
		t.Fatalf("expected 3 sink notifications, got %d", len(notified))
	}
	if events[0].Action != "created" || events[1].Action != "updated" || events[2].Action != "deleted" {
		t.Fatalf("unexpected actions: %s, %s, %s", events[0].Action, events[1].Action, events[2].Action)
	}
	if events[0].Payload["body"] != "hello" {
		t.Fatalf("created payload missing body: %#v", events[0].Payload)
	}
	if events[1].Payload["body"] != "edited" {
		t.Fatalf("updated payload missing new body: %#v", events[1].Payload)
	}
	if events[1].PreviousPayload["body"] != "hello" {
		t.Fatalf("updated previous payload missing old body: %#v", events[1].PreviousPayload)
	}
	if events[2].Payload["body"] != "edited" {
		t.Fatalf("deleted payload should contain deleted row: %#v", events[2].Payload)
	}
}
