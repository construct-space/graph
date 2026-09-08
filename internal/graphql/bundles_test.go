package graphql

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"construct-graph/internal/engine"
	"construct-graph/internal/schema"
)

// These tests exercise the publisher bundle features at the HTTP boundary —
// ServeHTTP + checkAccess + the _installs built-in. They are the complement
// to schema/bundles_test.go which covers migrations and persistence; here we
// lock down authorization decisions made on each request.

func newTestHandler(t *testing.T) (*Handler, *schema.Registry) {
	t.Helper()
	db, err := engine.Connect(":memory:")
	if err != nil {
		t.Fatalf("connect sqlite: %v", err)
	}
	reg := schema.NewRegistry(db)
	if err := reg.InitSystem(); err != nil {
		t.Fatalf("init: %v", err)
	}
	return NewHandler(reg, engine.New(db)), reg
}

// setupKanbanBundle publishes kanban (member access, bundle-attached) and
// returns the registry so tests can add installs / distribution tweaks.
func setupKanbanBundle(t *testing.T, reg *schema.Registry) {
	t.Helper()
	if err := reg.CreateSpaceBundle("kanban-suite", "Kanban Suite", "org-flak"); err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	if err := reg.RegisterWithOrg("kanban", "Kanban", "0.0.1",
		schema.Manifest{
			BundleID: "kanban-suite",
			Models: []schema.ModelDef{{
				Name:   "board",
				Fields: []schema.FieldDef{{Name: "title", Type: "string"}},
				Options: &schema.ModelOptions{
					Access: &schema.AccessRules{Read: "member", Create: "member", Update: "member", Delete: "member"},
				},
			}},
		},
		"default", "org-flak", "user-flak"); err != nil {
		t.Fatalf("publish kanban: %v", err)
	}
}

// graphqlReq builds a POST /graphql request with the auth headers our
// middleware normally injects. We don't wrap it in any real auth middleware
// here — the goal is to exercise the handler's decisions for given headers.
func graphqlReq(t *testing.T, spaceID, query string, headers map[string]string) *http.Request {
	t.Helper()
	return graphqlReqWithVars(t, spaceID, query, nil, headers)
}

// graphqlReqWithVars is the variables-aware variant. The handler's arg
// extractors today look at variables only (the inline `(spaceId: "...")` form
// is parsed as a label but not read by extractString).
func graphqlReqWithVars(t *testing.T, spaceID, query string, vars map[string]any, headers map[string]string) *http.Request {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	req := httptest.NewRequest("POST", "/graphql", bytes.NewReader(body))
	req.Header.Set("X-Space-ID", spaceID)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

func TestInstallGate_BlocksNonInstaller(t *testing.T) {
	h, reg := newTestHandler(t)
	setupKanbanBundle(t, reg)

	// Eve's org hasn't installed kanban. Request must be rejected before
	// reaching the resolver — we should see the install-gate's 403 with a
	// pointer to the install endpoint.
	rr := httptest.NewRecorder()
	req := graphqlReq(t, "kanban", "{ boards { id } }", map[string]string{
		"X-Auth-User-ID": "user-eve",
		"X-Auth-Org-ID":  "org-eve",
	})
	h.ServeHTTP(rr, req)

	body := readBody(t, rr.Result().Body)
	if rr.Code != 403 {
		t.Fatalf("expected 403, got %d — body=%s", rr.Code, body)
	}
	if !bytes.Contains(body, []byte("not installed")) {
		t.Fatalf("expected install-gate error, got: %s", body)
	}
}

func TestInstallGate_InstalledTenantPassesGate(t *testing.T) {
	h, reg := newTestHandler(t)
	setupKanbanBundle(t, reg)
	if err := reg.InstallSpace("kanban", "org-basecode"); err != nil {
		t.Fatalf("install: %v", err)
	}

	rr := httptest.NewRecorder()
	req := graphqlReq(t, "kanban", "{ boards { id } }", map[string]string{
		"X-Auth-User-ID": "user-basecode",
		"X-Auth-Org-ID":  "org-basecode",
	})
	h.ServeHTTP(rr, req)

	// The gate should pass — we don't care whether the actual query resolves
	// (empty table is fine) as long as we don't see the "not installed"
	// response or a 403 from the gate.
	body := readBody(t, rr.Result().Body)
	if bytes.Contains(body, []byte("not installed")) {
		t.Fatalf("install gate fired for an installed org: %s", body)
	}
}

func TestInstallGate_PublisherBypass(t *testing.T) {
	h, reg := newTestHandler(t)
	setupKanbanBundle(t, reg)

	// Publisher org needs no install row — they own the space.
	rr := httptest.NewRecorder()
	req := graphqlReq(t, "kanban", "{ boards { id } }", map[string]string{
		"X-Auth-User-ID": "user-flak",
		"X-Auth-Org-ID":  "org-flak",
	})
	h.ServeHTTP(rr, req)

	body := readBody(t, rr.Result().Body)
	if bytes.Contains(body, []byte("not installed")) {
		t.Fatalf("install gate fired for publisher: %s", body)
	}
}

func TestPublisherAdmin_RequiresRole(t *testing.T) {
	h, reg := newTestHandler(t)
	setupKanbanBundle(t, reg)

	// Publish an admin space that imports 'board' and requires publisher_admin.
	if err := reg.RegisterWithOrg("kanban-admin", "Kanban Admin", "0.0.1",
		schema.Manifest{
			BundleID: "kanban-suite",
			Imports:  []schema.ImportSpec{{From: "kanban", Models: []string{"board"}}},
			Models: []schema.ModelDef{{
				Name:   "board",
				Fields: []schema.FieldDef{{Name: "title", Type: "string"}},
				Options: &schema.ModelOptions{
					Access: &schema.AccessRules{
						Read: "publisher_admin", Create: "publisher_admin",
						Update: "publisher_admin", Delete: "publisher_admin",
					},
				},
			}},
		},
		"default", "org-flak", "user-flak"); err != nil {
		t.Fatalf("publish kanban-admin: %v", err)
	}

	// Caller is in publisher org but has NO admin role. Must be rejected.
	rr := httptest.NewRecorder()
	req := graphqlReq(t, "kanban-admin", "{ boards { id } }", map[string]string{
		"X-Auth-User-ID": "user-flak",
		"X-Auth-Org-ID":  "org-flak",
		// No X-Auth-Roles header.
	})
	h.ServeHTTP(rr, req)

	body := readBody(t, rr.Result().Body)
	if !bytes.Contains(body, []byte("lacks admin role")) {
		t.Fatalf("expected role-gate error, got: %s", body)
	}
}

func TestPublisherAdmin_WithRoleAllowed(t *testing.T) {
	h, reg := newTestHandler(t)
	setupKanbanBundle(t, reg)
	if err := reg.RegisterWithOrg("kanban-admin", "Kanban Admin", "0.0.1",
		schema.Manifest{
			BundleID: "kanban-suite",
			Imports:  []schema.ImportSpec{{From: "kanban", Models: []string{"board"}}},
			Models: []schema.ModelDef{{
				Name:   "board",
				Fields: []schema.FieldDef{{Name: "title", Type: "string"}},
				Options: &schema.ModelOptions{
					Access: &schema.AccessRules{Read: "publisher_admin"},
				},
			}},
		},
		"default", "org-flak", "user-flak"); err != nil {
		t.Fatalf("publish kanban-admin: %v", err)
	}

	rr := httptest.NewRecorder()
	req := graphqlReq(t, "kanban-admin", "{ boards { id } }", map[string]string{
		"X-Auth-User-ID": "user-flak",
		"X-Auth-Org-ID":  "org-flak",
		"X-Auth-Roles":   "developer,admin",
	})
	h.ServeHTTP(rr, req)

	body := readBody(t, rr.Result().Body)
	if bytes.Contains(body, []byte("lacks admin role")) {
		t.Fatalf("role gate wrongly fired: %s", body)
	}
	if bytes.Contains(body, []byte("not installed")) {
		t.Fatalf("install gate wrongly fired for publisher: %s", body)
	}
}

func TestBuiltinInstalls_PublisherOrgListsInstalls(t *testing.T) {
	h, reg := newTestHandler(t)
	setupKanbanBundle(t, reg)
	// Two tenants install kanban.
	if err := reg.InstallSpace("kanban", "org-basecode"); err != nil {
		t.Fatal(err)
	}
	if err := reg.InstallSpace("kanban", "org-urbanway"); err != nil {
		t.Fatal(err)
	}
	// Publish admin space that owns _installs access.
	if err := reg.RegisterWithOrg("kanban-admin", "Kanban Admin", "0.0.1",
		schema.Manifest{BundleID: "kanban-suite", Models: []schema.ModelDef{{Name: "noop"}}},
		"default", "org-flak", "user-flak"); err != nil {
		t.Fatalf("publish: %v", err)
	}

	rr := httptest.NewRecorder()
	req := graphqlReqWithVars(t, "kanban-admin",
		`query Installs($spaceId: String!) { _installs(spaceId: $spaceId) { orgId } }`,
		map[string]any{"spaceId": "kanban"},
		map[string]string{
			"X-Auth-User-ID": "user-flak",
			"X-Auth-Org-ID":  "org-flak",
			"X-Auth-Roles":   "admin",
		})
	h.ServeHTTP(rr, req)

	body := readBody(t, rr.Result().Body)
	if !bytes.Contains(body, []byte("org-basecode")) || !bytes.Contains(body, []byte("org-urbanway")) {
		t.Fatalf("expected both installing orgs in response, got: %s", body)
	}
}

func TestBuiltinInstalls_NonPublisherRejected(t *testing.T) {
	h, reg := newTestHandler(t)
	setupKanbanBundle(t, reg)

	rr := httptest.NewRecorder()
	// Eve tries to read installs of kanban from kanban itself. The built-in
	// gate requires caller.orgID == publisher_org_id.
	req := graphqlReq(t, "kanban", `{ _installs { orgId } }`, map[string]string{
		"X-Auth-User-ID": "user-eve",
		"X-Auth-Org-ID":  "org-eve",
	})
	// Need to install first so she clears the install gate.
	if err := reg.InstallSpace("kanban", "org-eve"); err != nil {
		t.Fatal(err)
	}
	h.ServeHTTP(rr, req)

	body := readBody(t, rr.Result().Body)
	if !bytes.Contains(body, []byte("restricted to the publisher org")) {
		t.Fatalf("expected publisher-org restriction error, got: %s", body)
	}
}

func readBody(t *testing.T, r io.ReadCloser) []byte {
	t.Helper()
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return b
}
