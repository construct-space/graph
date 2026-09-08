package graphql

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"construct-graph/internal/engine"
	"construct-graph/internal/schema"
)

// EventHub fans graph event ids from mutation writes to active realtime streams.
type EventHub struct {
	mu   sync.Mutex
	subs map[chan int64]struct{}
}

func NewEventHub() *EventHub {
	return &EventHub{subs: make(map[chan int64]struct{})}
}

func (h *EventHub) Publish(id int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- id:
		default:
		}
	}
}

func (h *EventHub) Subscribe() (chan int64, func()) {
	ch := make(chan int64, 16)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		close(ch)
		h.mu.Unlock()
	}
}

func (h *Handler) SetEventHub(hub *EventHub) {
	h.eventHub = hub
}

type streamEvent struct {
	ID             int64          `json:"id"`
	Action         string         `json:"action"`
	Model          string         `json:"model"`
	Record         map[string]any `json:"record,omitempty"`
	PreviousRecord map[string]any `json:"previous_record,omitempty"`
	CreatedAt      string         `json:"created_at,omitempty"`
}

func (h *Handler) ServeRealtime(w http.ResponseWriter, r *http.Request) {
	model := sanitizeGQL(r.URL.Query().Get("model"))
	if model == "" {
		writeError(w, "model query parameter required", http.StatusBadRequest)
		return
	}

	rctx, status, msg := h.realtimeRequestContext(r)
	if status != 0 {
		writeError(w, msg, status)
		return
	}
	if err := h.checkAccess(rctx, model, "read"); err != nil {
		writeError(w, err.Error(), http.StatusForbidden)
		return
	}

	where := parseRealtimeWhere(r.URL.Query().Get("where"))
	vars := h.applyOwnerFilter(rctx, model, map[string]any{"where": where})
	if filtered, ok := vars["where"].(map[string]any); ok {
		where = filtered
	}

	cursor, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
	modelSchema := h.resolveSchemaForModel(rctx, model)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	notifyCh, unsubscribe := h.subscribeEvents()
	defer unsubscribe()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	poll := time.NewTicker(1 * time.Second)
	defer poll.Stop()

	sendBacklog := func(ctx context.Context) bool {
		for {
			events, err := h.engine.ListGraphEvents(ctx, modelSchema, model, cursor, 250)
			if err != nil {
				fmt.Fprintf(w, "event: error\ndata: %q\n\n", err.Error())
				flusher.Flush()
				return false
			}
			if len(events) == 0 {
				return true
			}
			for _, ev := range events {
				cursor = ev.ID
				if !matchesRealtimeWhere(where, ev) {
					continue
				}
				if !writeRealtimeEvent(w, ev) {
					return false
				}
				flusher.Flush()
			}
			if len(events) < 250 {
				return true
			}
		}
	}

	ctx := r.Context()
	if !sendBacklog(ctx) {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-notifyCh:
			if !sendBacklog(ctx) {
				return
			}
		case <-poll.C:
			if !sendBacklog(ctx) {
				return
			}
		case <-heartbeat.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func (h *Handler) subscribeEvents() (<-chan int64, func()) {
	if h.eventHub == nil {
		return make(chan int64), func() {}
	}
	return h.eventHub.Subscribe()
}

func (h *Handler) realtimeRequestContext(r *http.Request) (*requestContext, int, string) {
	spaceID := r.Header.Get("X-Space-ID")
	projectID := r.Header.Get("X-Project-ID")
	if spaceID == "" {
		return nil, http.StatusBadRequest, "X-Space-ID header required"
	}
	if projectID == "" {
		projectID = "default"
	}

	userID := r.Header.Get("X-Auth-User-ID")
	authScope := r.Header.Get("X-Auth-Scope")
	authOrgID := r.Header.Get("X-Auth-Org-ID")

	spaceScope := h.registry.GetSpaceScope(spaceID, authOrgID)
	orgID, status, msg := resolveOrgID(spaceScope, authScope, authOrgID)
	if status != 0 {
		return nil, status, msg
	}
	if authOrgID != "" {
		if publisher := h.registry.GetSpacePublisherOrg(spaceID); publisher != "" && publisher != authOrgID {
			if !h.registry.IsInstalled(spaceID, authOrgID) {
				return nil, http.StatusForbidden, "space not installed for this org"
			}
		}
	}

	schemaName := schema.ResolveSchemaName(spaceScope, spaceID, projectID, orgID, userID)
	if spaceScope != "org" && spaceScope != "app" {
		if existingSchema, err := h.registry.GetSchemaName(spaceID, projectID); err == nil && existingSchema != "" {
			schemaName = existingSchema
		}
	} else if err := h.registry.EnsureTenantSchema(spaceID, schemaName); err != nil {
		return nil, http.StatusInternalServerError, fmt.Sprintf("provision tenant schema: %v", err)
	}

	var roles []string
	if raw := r.Header.Get("X-Auth-Roles"); raw != "" {
		for _, role := range strings.Split(raw, ",") {
			if s := strings.TrimSpace(role); s != "" {
				roles = append(roles, s)
			}
		}
	}
	return &requestContext{
		spaceID:   spaceID,
		projectID: projectID,
		userID:    userID,
		orgID:     authOrgID,
		roles:     roles,
		schema:    schemaName,
	}, 0, ""
}

func parseRealtimeWhere(raw string) map[string]any {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}
	}
	var where map[string]any
	if json.Unmarshal([]byte(raw), &where) != nil {
		return map[string]any{}
	}
	return where
}

func matchesRealtimeWhere(where map[string]any, ev engine.GraphEvent) bool {
	return recordMatches(where, ev.Payload) || recordMatches(where, ev.PreviousPayload)
}

func recordMatches(where map[string]any, record map[string]any) bool {
	if len(where) == 0 {
		return true
	}
	if record == nil {
		return false
	}
	for field, expected := range where {
		actual, ok := record[field]
		if !ok {
			return false
		}
		if !valueMatches(actual, expected) {
			return false
		}
	}
	return true
}

func valueMatches(actual, expected any) bool {
	if op, ok := expected.(map[string]any); ok {
		for name, value := range op {
			switch name {
			case "$ne":
				if fmt.Sprint(actual) == fmt.Sprint(value) {
					return false
				}
			case "$in":
				values, ok := value.([]any)
				if !ok {
					return false
				}
				found := false
				for _, candidate := range values {
					if fmt.Sprint(actual) == fmt.Sprint(candidate) {
						found = true
						break
					}
				}
				if !found {
					return false
				}
			default:
				return false
			}
		}
		return true
	}
	return fmt.Sprint(actual) == fmt.Sprint(expected)
}

func writeRealtimeEvent(w http.ResponseWriter, ev engine.GraphEvent) bool {
	data, err := json.Marshal(streamEvent{
		ID:             ev.ID,
		Action:         ev.Action,
		Model:          ev.TableName,
		Record:         ev.Payload,
		PreviousRecord: ev.PreviousPayload,
		CreatedAt:      ev.CreatedAt,
	})
	if err != nil {
		return false
	}
	fmt.Fprintf(w, "id: %d\n", ev.ID)
	fmt.Fprint(w, "event: graph_event\n")
	fmt.Fprintf(w, "data: %s\n\n", data)
	return true
}
