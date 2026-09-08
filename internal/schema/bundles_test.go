package schema

import (
	"testing"

	"construct-graph/internal/engine"
)

// These tests exercise the publisher/bundle/install path against an in-memory
// SQLite DB. The SQL builder and migrations are the risk — if ownership checks
// leak across orgs, another dev can hijack a bundle or read installs they
// shouldn't see. Keep this coverage tight.

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	db, err := engine.Connect(":memory:")
	if err != nil {
		t.Fatalf("connect in-memory sqlite: %v", err)
	}
	r := NewRegistry(db)
	if err := r.InitSystem(); err != nil {
		t.Fatalf("init system: %v", err)
	}
	return r
}

func TestSpaceBundle_CreateAndOwnerGate(t *testing.T) {
	r := newTestRegistry(t)

	if err := r.CreateSpaceBundle("kanban-suite", "Kanban Suite", "org-flak"); err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	// Duplicate rejected.
	if err := r.CreateSpaceBundle("kanban-suite", "Different Name", "org-flak"); err == nil {
		t.Fatal("expected duplicate bundle to error")
	}
	// Lookup returns the owner.
	b, err := r.GetSpaceBundle("kanban-suite")
	if err != nil {
		t.Fatalf("get bundle: %v", err)
	}
	if b == nil || b.OwnerOrgID != "org-flak" {
		t.Fatalf("unexpected bundle: %+v", b)
	}
}

func TestRegister_BundleOwnerMismatchRejected(t *testing.T) {
	r := newTestRegistry(t)

	if err := r.CreateSpaceBundle("kanban-suite", "Kanban Suite", "org-flak"); err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	// Another org tries to attach a new space to Flak's bundle. MUST fail.
	err := r.RegisterWithOrg("kanban-spy", "Spy", "0.0.1",
		Manifest{BundleID: "kanban-suite", Models: []ModelDef{{Name: "stuff"}}},
		"default", "org-eve", "user-eve")
	if err == nil {
		t.Fatal("expected bundle hijack to be rejected")
	}
}

func TestRegister_BundleOwnerMatchAllowed(t *testing.T) {
	r := newTestRegistry(t)

	if err := r.CreateSpaceBundle("kanban-suite", "Kanban Suite", "org-flak"); err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	err := r.RegisterWithOrg("kanban", "Kanban", "0.0.1",
		Manifest{BundleID: "kanban-suite", Models: []ModelDef{{Name: "board", Fields: []FieldDef{{Name: "title", Type: "string"}}}}},
		"default", "org-flak", "user-flak")
	if err != nil {
		t.Fatalf("register own bundle: %v", err)
	}
	if bid := r.GetSpaceBundleIDFor("kanban"); bid != "kanban-suite" {
		t.Fatalf("expected bundle_id=kanban-suite, got %q", bid)
	}
	if pub := r.GetSpacePublisherOrg("kanban"); pub != "org-flak" {
		t.Fatalf("expected publisher_org_id=org-flak, got %q", pub)
	}
}

func TestImport_SameBundleResolves(t *testing.T) {
	r := newTestRegistry(t)
	if err := r.CreateSpaceBundle("kanban-suite", "Kanban Suite", "org-flak"); err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	// Publish kanban first so kanban-admin can import from it.
	if err := r.RegisterWithOrg("kanban", "Kanban", "0.0.1",
		Manifest{BundleID: "kanban-suite", Models: []ModelDef{{Name: "board", Fields: []FieldDef{{Name: "title", Type: "string"}}}}},
		"default", "org-flak", "user-flak"); err != nil {
		t.Fatalf("publish kanban: %v", err)
	}
	if err := r.RegisterWithOrg("kanban-admin", "Kanban Admin", "0.0.1",
		Manifest{
			BundleID: "kanban-suite",
			Imports:  []ImportSpec{{From: "kanban", Models: []string{"board"}}},
		},
		"default", "org-flak", "user-flak"); err != nil {
		t.Fatalf("publish kanban-admin: %v", err)
	}

	// Imported lookup: kanban-admin → board should resolve to kanban.
	resolved := r.ResolveModel("kanban-admin", "board")
	if resolved == nil {
		t.Fatal("expected ResolveModel to find imported board")
	}
	if !resolved.Imported || resolved.SourceSpaceID != "kanban" {
		t.Fatalf("expected imported from kanban, got %+v", resolved)
	}
	if resolved.AccessSpaceID != "kanban-admin" {
		t.Fatalf("expected access via kanban-admin, got %q", resolved.AccessSpaceID)
	}
}

func TestImport_CrossBundleRejected(t *testing.T) {
	r := newTestRegistry(t)
	// Two separate bundles, two separate publisher orgs.
	if err := r.CreateSpaceBundle("kanban-suite", "Kanban", "org-flak"); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateSpaceBundle("crm-suite", "CRM", "org-eve"); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterWithOrg("kanban", "Kanban", "0.0.1",
		Manifest{BundleID: "kanban-suite", Models: []ModelDef{{Name: "board"}}},
		"default", "org-flak", "user-flak"); err != nil {
		t.Fatalf("publish kanban: %v", err)
	}
	// Eve tries to import kanban's board into her crm space. MUST fail.
	err := r.RegisterWithOrg("crm-hack", "CRM Hack", "0.0.1",
		Manifest{
			BundleID: "crm-suite",
			Imports:  []ImportSpec{{From: "kanban", Models: []string{"board"}}},
		},
		"default", "org-eve", "user-eve")
	if err == nil {
		t.Fatal("expected cross-bundle import to be rejected")
	}
}

func TestInstall_DistributionGates(t *testing.T) {
	r := newTestRegistry(t)
	if err := r.CreateSpaceBundle("kanban-suite", "Kanban", "org-flak"); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterWithOrg("kanban", "Kanban", "0.0.1",
		Manifest{BundleID: "kanban-suite", Models: []ModelDef{{Name: "board"}}},
		"default", "org-flak", "user-flak"); err != nil {
		t.Fatalf("publish kanban: %v", err)
	}

	// Default distribution is "public" — any org can install.
	if err := r.InstallSpace("kanban", "org-basecode"); err != nil {
		t.Fatalf("public install: %v", err)
	}
	if !r.IsInstalled("kanban", "org-basecode") {
		t.Fatal("expected basecode to be installed after public install")
	}

	// Switch to private — only publisher may install; other orgs blocked.
	if err := r.SetSpaceDistribution("kanban", DistributionPrivate); err != nil {
		t.Fatalf("set private: %v", err)
	}
	if err := r.InstallSpace("kanban", "org-urbanway"); err == nil {
		t.Fatal("expected private install to be rejected for non-publisher")
	}
	// Publisher is implicitly installed even for private.
	if !r.IsInstalled("kanban", "org-flak") {
		t.Fatal("expected publisher to be implicitly installed")
	}

	// Allowlist mode — only listed orgs may install.
	if err := r.SetSpaceDistribution("kanban", DistributionOrgAllowlist); err != nil {
		t.Fatalf("set allowlist: %v", err)
	}
	if err := r.InstallSpace("kanban", "org-urbanway"); err == nil {
		t.Fatal("expected allowlist install to reject unlisted org")
	}
	if err := r.AddToAllowlist("kanban", "org-urbanway"); err != nil {
		t.Fatal(err)
	}
	if err := r.InstallSpace("kanban", "org-urbanway"); err != nil {
		t.Fatalf("expected allowlisted install to succeed: %v", err)
	}
}

func TestInstall_ListReturnsInstalledOrgs(t *testing.T) {
	r := newTestRegistry(t)
	if err := r.CreateSpaceBundle("kanban-suite", "Kanban", "org-flak"); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterWithOrg("kanban", "Kanban", "0.0.1",
		Manifest{BundleID: "kanban-suite", Models: []ModelDef{{Name: "board"}}},
		"default", "org-flak", "user-flak"); err != nil {
		t.Fatal(err)
	}
	if err := r.InstallSpace("kanban", "org-basecode"); err != nil {
		t.Fatal(err)
	}
	if err := r.InstallSpace("kanban", "org-urbanway"); err != nil {
		t.Fatal(err)
	}
	orgs, err := r.ListInstalls("kanban")
	if err != nil {
		t.Fatalf("list installs: %v", err)
	}
	if len(orgs) != 2 {
		t.Fatalf("expected 2 orgs, got %d: %v", len(orgs), orgs)
	}
}
