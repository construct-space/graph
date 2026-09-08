package schema

import (
	"strings"

	"construct-graph/internal/engine"
)

// ModelDef represents a data model defined by a space
type ModelDef struct {
	Name    string        `json:"name"`
	Fields  []FieldDef    `json:"fields"`
	Options *ModelOptions `json:"options,omitempty"`
}

// FieldDef represents a field in a model
type FieldDef struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"` // string, int, number, boolean, date, enum, json, relation
	Required   bool     `json:"required,omitempty"`
	Unique     bool     `json:"unique,omitempty"`
	Default    any      `json:"default,omitempty"`
	Index      bool     `json:"index,omitempty"`
	Validation string   `json:"validation,omitempty"` // email, url, etc.
	Values     []string `json:"values,omitempty"`     // for enum type
	// Relation fields
	Relation string `json:"relation,omitempty"`  // belongsTo, hasMany
	Target   string `json:"target,omitempty"`    // target model name
	OnDelete string `json:"on_delete,omitempty"` // cascade, set_null, restrict
	Nullable bool   `json:"nullable,omitempty"`
}

// AccessRules defines per-operation access levels
type AccessRules struct {
	Read   string `json:"read"` // public, authenticated, owner, member, admin, none
	Create string `json:"create"`
	Update string `json:"update"`
	Delete string `json:"delete"`
}

// ModelOptions holds scope and access rules for a model.
//
// Scopes is the current vocabulary: an array containing one or both of
// "app" (per-user partition) and "org" (per-org partition). When both are
// declared the runtime picks the right partition from the caller's
// session — an authenticated org context uses "org", otherwise "app".
//
// Scope is the legacy singular field. Kept for spaces still on the old
// SDK; new spaces send Scopes only.
type ModelOptions struct {
	Scope  string       `json:"scope,omitempty"`
	Scopes []string     `json:"scopes,omitempty"`
	Access *AccessRules `json:"access,omitempty"`
}

// Manifest represents the data.manifest.json from a space
type Manifest struct {
	Version int        `json:"version"`
	Models  []ModelDef `json:"models"`

	// BundleID groups related spaces published by the same org (e.g. kanban +
	// kanban-admin as "Kanban Suite"). Required for cross-space imports.
	// Empty string means the space is standalone (legacy). Distinct from the
	// consumer-side project_id which identifies a tenant workspace.
	BundleID string `json:"bundle_id,omitempty"`

	// Imports declares models from sibling spaces in the same bundle. Each
	// source space must share this space's bundle_id. Imported models are
	// resolved at query time against the source space's schema, with access
	// rules overridden by the importing space's own manifest if it redeclares
	// the model.
	Imports []ImportSpec `json:"imports,omitempty"`
}

// ImportSpec declares that this space re-uses models from another space.
// From is the source space_id; Models lists the specific model names to pull
// in (must exist in the source manifest).
type ImportSpec struct {
	From   string   `json:"from"`
	Models []string `json:"models"`
}

// SpaceBundle groups one or more spaces published by the same org (e.g. a
// "Kanban Suite" bundle containing `kanban` and `kanban-admin`). Used as the
// boundary for cross-space imports and publisher-admin access.
type SpaceBundle struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	OwnerOrgID string `json:"owner_org_id"`
	CreatedAt  string `json:"created_at,omitempty"`
}

// SpaceSummary is the shape returned by the /api/spaces list endpoint —
// enough detail for a publisher dashboard or CLI listing without pulling the
// full manifest. InstallCount does NOT count the implicit publisher install.
type SpaceSummary struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	LatestVersion  string `json:"latest_version"`
	BundleID       string `json:"bundle_id,omitempty"`
	Distribution   string `json:"distribution"`
	PublisherOrgID string `json:"publisher_org_id,omitempty"`
	InstallCount   int    `json:"install_count"`
}

// ModelDef represents a data model defined by a space
// (moved from above for grouping — keeping original for fields)

// SchemaName returns the schema name for a space+project (standalone/project scope)
func SchemaName(spaceID, projectID string) string {
	return "s_" + sanitize(spaceID) + "_p_" + sanitize(projectID)
}

// OrgSchemaName returns the schema name for an org-scoped space (per-org bucket)
func OrgSchemaName(orgID, spaceID string) string {
	return "o_" + sanitize(orgID) + "_s_" + sanitize(spaceID)
}

// UserSchemaName returns the schema name for an app-scoped space (per-user bucket)
func UserSchemaName(userID, spaceID string) string {
	return "u_" + sanitize(userID) + "_s_" + sanitize(spaceID)
}

// ResolveSchemaName returns the correct schema name based on scope.
//
// Scope vocabulary (matches the host space-manifest scope vocabulary):
//
//	"org"     — per-org bucket. Falls back to per-user when caller has no org.
//	"app"     — per-user bucket.
//	"project" — per-space+project bucket; default for legacy callers.
func ResolveSchemaName(scope, spaceID, projectID, orgID, userID string) string {
	switch scope {
	case "org":
		if orgID != "" {
			return OrgSchemaName(orgID, spaceID)
		}
		// Personal caller (no org) — isolate in their own user bucket
		// instead of bleeding into the shared project partition.
		if userID != "" {
			return UserSchemaName(userID, spaceID)
		}
		return SchemaName(spaceID, projectID)
	case "app":
		if userID != "" {
			return UserSchemaName(userID, spaceID)
		}
		return SchemaName(spaceID, projectID)
	default:
		// No app/org scope declared (legacy/standalone). Prefer per-user
		// isolation so a scope-less space can't drop an authenticated caller
		// into a shared partition keyed by a client-supplied projectID. Only
		// truly anonymous callers fall back to the shared project partition.
		if userID != "" {
			return UserSchemaName(userID, spaceID)
		}
		return SchemaName(spaceID, projectID)
	}
}

// quoteIdent wraps an identifier in double quotes so reserved SQL keywords
// (table, order, user, etc.) can be used as column or table names. Both
// PostgreSQL and SQLite accept the SQL-standard "name" form.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func sanitize(s string) string {
	out := make([]byte, 0, len(s))
	for _, b := range []byte(s) {
		if (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_' {
			out = append(out, b)
		} else if b >= 'A' && b <= 'Z' {
			out = append(out, b+32) // lowercase
		} else if b == '-' {
			out = append(out, '_')
		}
	}
	return string(out)
}

// FieldTypeToSQL maps field types to SQL column types
func FieldTypeToSQL(f FieldDef) string {
	pg := engine.IsPostgres()
	switch f.Type {
	case "string":
		return "TEXT"
	case "int":
		return "INTEGER"
	case "number":
		if pg {
			return "NUMERIC"
		}
		return "REAL"
	case "boolean":
		if pg {
			return "BOOLEAN"
		}
		return "INTEGER" // SQLite uses 0/1
	case "date":
		if pg {
			return "TIMESTAMPTZ"
		}
		return "DATETIME"
	case "enum":
		return "TEXT"
	case "json":
		if pg {
			return "JSONB"
		}
		return "TEXT"
	case "relation":
		if pg {
			return "UUID"
		}
		return "TEXT" // SQLite stores UUIDs as text
	default:
		return "TEXT"
	}
}

// TimestampType returns the appropriate timestamp type
func TimestampType() string {
	if engine.IsPostgres() {
		return "TIMESTAMPTZ"
	}
	return "DATETIME"
}

// IDColumn returns the ID column definition
func IDColumn() string {
	if engine.IsPostgres() {
		return `"id" UUID PRIMARY KEY DEFAULT gen_random_uuid()`
	}
	return `"id" TEXT PRIMARY KEY`
}

// NowDefault returns the default NOW expression
func NowDefault() string {
	if engine.IsPostgres() {
		return "DEFAULT now()"
	}
	return "DEFAULT (datetime('now'))"
}

// SerialPK returns auto-increment primary key
func SerialPK() string {
	if engine.IsPostgres() {
		return "SERIAL PRIMARY KEY"
	}
	return "INTEGER PRIMARY KEY AUTOINCREMENT"
}
