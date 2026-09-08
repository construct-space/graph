package graphql

import "testing"

// Scope isolation is the most security-sensitive decision in the graph
// handler: a bug here means org A reads org B's data. These tests exercise
// resolveOrgID — the single function that decides which org's schema a
// request targets — across the full matrix of (space scope × auth scope ×
// auth org present).

func TestResolveOrgID_OrgScope_PersonalCallerSucceedsWithEmptyOrgID(t *testing.T) {
	// Personal callers (user scope, no org id) on an org-scoped space are
	// allowed through with empty orgID — ResolveSchemaName then routes them
	// to a per-user bucket so they don't share the standalone partition with
	// other personal users.
	cases := []struct {
		name      string
		authScope string
		authOrgID string
	}{
		{"no auth scope at all", "", ""},
		{"user scope no org", "user", ""},
		{"user scope with stray org id", "user", "acme"},
		{"org scope but no org id", "org", ""},
		{"unknown scope literal", "something-weird", "acme"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orgID, status, msg := resolveOrgID("org", tc.authScope, tc.authOrgID)
			if status != 0 {
				t.Fatalf("expected status 0 for personal caller on org-scoped space, got %d (%q)", status, msg)
			}
			if orgID != "" {
				t.Fatalf("expected empty orgID for personal caller, got %q", orgID)
			}
		})
	}
}

func TestResolveOrgID_OrgScope_TrustsAuthOrgID(t *testing.T) {
	// The critical security property: when scope=org and the session
	// carries an authenticated org ID, the handler MUST use that ID. Client-
	// supplied X-Company-ID headers are ignored upstream.
	orgID, status, msg := resolveOrgID("org", "org", "acme-uuid")
	if status != 0 {
		t.Fatalf("expected success, got status %d (%q)", status, msg)
	}
	if orgID != "acme-uuid" {
		t.Fatalf("expected orgID=acme-uuid, got %q", orgID)
	}
}

func TestResolveOrgID_NonOrgScope_IgnoresOrg(t *testing.T) {
	// Project / app / unknown scopes should never use orgID — they're keyed
	// by project or user, not by org.
	for _, scope := range []string{"", "project", "app", "weird"} {
		t.Run("scope="+scope, func(t *testing.T) {
			orgID, status, _ := resolveOrgID(scope, "org", "acme-uuid")
			if status != 0 {
				t.Fatalf("expected success, got %d", status)
			}
			if orgID != "" {
				t.Fatalf("expected empty orgID for scope=%q, got %q", scope, orgID)
			}
		})
	}
}

// TestResolveOrgID_ClientCannotOverrideOrg documents the leakage
// prevention: no matter what X-Company-ID a client sends, this function
// derives the org ID only from the (trusted) auth inputs.
func TestResolveOrgID_ClientCannotOverrideOrg(t *testing.T) {
	// Attacker scenario: authenticated as member of org A, tries to read
	// org B's data by setting X-Company-ID=org-B on the request. The
	// middleware writes X-Auth-Org-ID=org-A from /api/me/scope; that's
	// what reaches resolveOrgID. Result: targets org A, not org B.
	victimOrg := "org-A"
	orgID, status, _ := resolveOrgID("org", "org", victimOrg)
	if status != 0 {
		t.Fatalf("expected success, got %d", status)
	}
	if orgID != victimOrg {
		t.Fatalf("expected orgID=%q, got %q — scope resolution is trusting an unsafe source", victimOrg, orgID)
	}
}
