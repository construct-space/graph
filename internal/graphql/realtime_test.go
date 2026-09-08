package graphql

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"construct-graph/internal/schema"
)

func TestRealtimeStreamReplaysMatchingEvents(t *testing.T) {
	h, reg := newTestHandler(t)
	if err := reg.Register("chat", "Chat", "0.0.1", schema.Manifest{
		Models: []schema.ModelDef{{
			Name: "message",
			Fields: []schema.FieldDef{
				{Name: "room_id", Type: "string", Index: true},
				{Name: "body", Type: "string"},
			},
			Options: &schema.ModelOptions{
				Access: &schema.AccessRules{Read: "authenticated", Create: "authenticated", Update: "authenticated", Delete: "authenticated"},
			},
		}},
	}, "default", "user-a"); err != nil {
		t.Fatalf("register: %v", err)
	}

	schemaName := schema.SchemaName("chat", "default")
	if _, err := h.engine.Create(context.Background(), schemaName, "message", map[string]any{
		"room_id":    "general",
		"body":       "hello",
		"created_by": "user-a",
	}); err != nil {
		t.Fatalf("create message: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(h.ServeRealtime))
	defer srv.Close()

	where := url.QueryEscape(`{"room_id":"general"}`)
	req, err := http.NewRequest("GET", srv.URL+"/realtime/stream?model=message&where="+where+"&cursor=0", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("X-Space-ID", "chat")
	req.Header.Set("X-Project-ID", "default")
	req.Header.Set("X-Auth-User-ID", "user-a")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("expected event stream content type, got %q", ct)
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event struct {
			Action string         `json:"action"`
			Record map[string]any `json:"record"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		if event.Action != "created" {
			t.Fatalf("expected created event, got %q", event.Action)
		}
		if event.Record["body"] != "hello" {
			t.Fatalf("expected body hello, got %#v", event.Record)
		}
		return
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan stream: %v", err)
	}
	t.Fatal("stream ended without graph event")
}
