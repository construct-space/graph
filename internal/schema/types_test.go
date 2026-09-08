package schema

import "testing"

// These tests lock in the physical isolation contract:
//   - Two different org IDs MUST resolve to different schemas.
//   - Two different user IDs (personal callers on org-scoped spaces) MUST
//     resolve to different schemas.
//   - Project scope MUST ignore both org and user IDs.
// If any of these drift, callers from one tenant could read another's rows.

func TestOrgScope_DifferentOrgsGetDifferentSchemas(t *testing.T) {
	a := ResolveSchemaName("org", "kanban", "default", "org-a", "")
	b := ResolveSchemaName("org", "kanban", "default", "org-b", "")
	if a == b {
		t.Fatalf("same schema for different orgs: %q", a)
	}
	if a == "" || b == "" {
		t.Fatalf("expected non-empty schemas, got %q / %q", a, b)
	}
}

func TestOrgScope_IncludesOrgInName(t *testing.T) {
	// The 'o_<org>_s_<space>' convention is how downstream queries know
	// which org's data to touch. If the org ID is missing from the name,
	// the handler would route to a shared schema — a cross-org leak.
	s := ResolveSchemaName("org", "kanban", "default", "acme", "")
	if s != OrgSchemaName("acme", "kanban") {
		t.Fatalf("expected OrgSchemaName, got %q", s)
	}
}

func TestProjectScope_IgnoresOrgAndUser(t *testing.T) {
	// Project scope must never embed orgID or userID. If it did, setting
	// X-Auth-Org-ID would move a personal user's data into an org's schema
	// without their consent.
	a := ResolveSchemaName("project", "kanban", "default", "org-a", "user-a")
	b := ResolveSchemaName("project", "kanban", "default", "org-b", "user-b")
	noOrg := ResolveSchemaName("project", "kanban", "default", "", "")
	if a != b || a != noOrg {
		t.Fatalf("project schema varied by org/user: %q / %q / %q", a, b, noOrg)
	}
}

func TestOrgScope_PersonalCallerIsolatedPerUser(t *testing.T) {
	// Personal callers (no orgID) on an org-scoped space must NOT all share
	// the standalone schema — that's the cross-tenant leak we just fixed.
	// They each get their own per-user bucket so two personal users can't
	// see each other's events.
	a := ResolveSchemaName("org", "calendar", "default", "", "user-a")
	b := ResolveSchemaName("org", "calendar", "default", "", "user-b")
	if a == b {
		t.Fatalf("same schema for different personal users on org-scoped space: %q", a)
	}
	if a != UserSchemaName("user-a", "calendar") {
		t.Fatalf("expected UserSchemaName fallback, got %q", a)
	}
}

func TestOrgScope_EmptyOrgAndEmptyUserFallsBackSafely(t *testing.T) {
	// Defense-in-depth: with neither orgID nor userID, fall back to the
	// shared project schema rather than a broken "o__s_..." name. The
	// handler should normally provide at least a userID before reaching here.
	s := ResolveSchemaName("org", "kanban", "default", "", "")
	if s != SchemaName("kanban", "default") {
		t.Fatalf("expected fallback to project schema, got %q", s)
	}
}

func TestAppScope_PerUserBucket(t *testing.T) {
	// App scope = per-user bucket. Two users on the same space MUST get
	// different schemas; the same user across orgs gets the same schema
	// (orgID is ignored for app scope).
	a := ResolveSchemaName("app", "notes", "default", "", "user-a")
	b := ResolveSchemaName("app", "notes", "default", "", "user-b")
	sameUserDifferentOrg := ResolveSchemaName("app", "notes", "default", "any-org", "user-a")
	if a == b {
		t.Fatalf("same schema for different users on app-scoped space: %q", a)
	}
	if a != sameUserDifferentOrg {
		t.Fatalf("app scope leaked org context: %q vs %q", a, sameUserDifferentOrg)
	}
}

func TestSanitize_NeverProducesSQLBreaks(t *testing.T) {
	// Org IDs come from our own DB (UUIDs) but defense in depth: sanitize
	// strips anything that isn't [a-z0-9_]. Prevents a schema-name
	// injection if any upstream ever lets user input reach here.
	bad := sanitize("org--a;DROP SCHEMA foo--")
	for _, c := range bad {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_') {
			t.Fatalf("sanitize produced unsafe char %q in %q", c, bad)
		}
	}
}
