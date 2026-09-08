package schema

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strings"
	"time"

	"construct-graph/internal/engine"

	"gorm.io/gorm"
)

// createSchemaIfNotExistsSafe creates a Postgres schema, tolerating concurrent
// calls. Postgres' "CREATE SCHEMA IF NOT EXISTS" is *not* concurrency-safe at
// the catalog level: two simultaneous calls can both pass the existence check
// and then race on inserting into pg_namespace, surfacing as
// "duplicate key value violates unique constraint pg_namespace_nspname_index".
//
// We serialize per schema-name with a transaction-scoped advisory lock keyed
// by an FNV-1a hash of the name. The lock is released automatically when the
// transaction commits.
//
// Caller is responsible for the engine.IsPostgres() check; this is a no-op on
// SQLite (which has no schemas).
func (r *Registry) createSchemaIfNotExistsSafe(schemaName string) error {
	if !engine.IsPostgres() {
		return nil
	}
	h := fnv.New64a()
	h.Write([]byte(schemaName))
	lockKey := int64(h.Sum64())

	return r.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", lockKey).Error; err != nil {
			return fmt.Errorf("acquire advisory lock for %s: %w", schemaName, err)
		}
		if err := tx.Exec(fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", schemaName)).Error; err != nil {
			return fmt.Errorf("create schema %s: %w", schemaName, err)
		}
		return nil
	})
}

// createTableIfNotExistsSafe runs a CREATE TABLE IF NOT EXISTS under a
// transaction-scoped advisory lock keyed by schema+table. Postgres'
// CREATE TABLE (even with IF NOT EXISTS) is *not* concurrency-safe at the
// catalog level — two simultaneous creates of the same tenant table race on
// pg_class/pg_type and one fails with "duplicate key value violates unique
// constraint". Spaces load several models in parallel (Promise.all), so the
// first request for a fresh tenant schema hits exactly this. Serialize per
// table-name, same as createSchemaIfNotExistsSafe does for schemas. No-op lock
// on SQLite (single-writer, no shared catalog).
func (r *Registry) createTableIfNotExistsSafe(schemaName, tableName, createSQL string) error {
	if !engine.IsPostgres() {
		return r.db.Exec(createSQL).Error
	}
	h := fnv.New64a()
	h.Write([]byte(schemaName + "." + tableName))
	lockKey := int64(h.Sum64())

	return r.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", lockKey).Error; err != nil {
			return fmt.Errorf("acquire advisory lock for %s.%s: %w", schemaName, tableName, err)
		}
		return tx.Exec(createSQL).Error
	})
}

// Registry manages space schemas and model definitions
type Registry struct {
	db *gorm.DB
}

func NewRegistry(db *gorm.DB) *Registry {
	return &Registry{db: db}
}

// systemTable returns the table name for a _system table
func systemTable(name string) string {
	return engine.TableRef("_system", name)
}

// InitSystem creates the _system schema and tables
func (r *Registry) InitSystem() error {
	if engine.IsPostgres() {
		return r.initSystemPostgres()
	}
	return r.initSystemSQLite()
}

func (r *Registry) initSystemPostgres() error {
	return r.db.Exec(`
		CREATE SCHEMA IF NOT EXISTS _system;

		CREATE TABLE IF NOT EXISTS _system.spaces (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			latest_version TEXT NOT NULL DEFAULT '0.0.0',
			owner_user_id TEXT,
			registered_at TIMESTAMPTZ DEFAULT now()
		);

		-- Backfill: add owner_user_id column if table existed before this migration
		ALTER TABLE _system.spaces ADD COLUMN IF NOT EXISTS owner_user_id TEXT;

		-- Publisher grouping: bundle_id links related spaces (e.g. kanban +
		-- kanban-admin as "Kanban Suite"). publisher_org_id is the org that
		-- owns all spaces in the bundle. Both nullable for legacy standalone
		-- spaces.
		ALTER TABLE _system.spaces ADD COLUMN IF NOT EXISTS bundle_id TEXT;
		ALTER TABLE _system.spaces ADD COLUMN IF NOT EXISTS publisher_org_id TEXT;

		CREATE TABLE IF NOT EXISTS _system.space_bundles (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			owner_org_id TEXT NOT NULL,
			created_at TIMESTAMPTZ DEFAULT now()
		);

		CREATE INDEX IF NOT EXISTS idx_spaces_bundle_id ON _system.spaces(bundle_id);
		CREATE INDEX IF NOT EXISTS idx_space_bundles_owner_org_id ON _system.space_bundles(owner_org_id);

		-- space_imports: each row grants space_id read access to one model in
		-- from_space_id. Publish-time validation ensures both spaces share a
		-- product_id; a runtime resolver (ResolveModel) follows these rows.
		CREATE TABLE IF NOT EXISTS _system.space_imports (
			space_id TEXT NOT NULL,
			from_space_id TEXT NOT NULL,
			model_name TEXT NOT NULL,
			created_at TIMESTAMPTZ DEFAULT now(),
			PRIMARY KEY (space_id, model_name)
		);
		CREATE INDEX IF NOT EXISTS idx_space_imports_from ON _system.space_imports(from_space_id);

		-- Distribution mode controls who may install the space.
		-- public        — any org may install (default, marketplace)
		-- org_allowlist — only orgs in _system.space_allowlist may install
		-- private       — only the publisher_org_id may install
		ALTER TABLE _system.spaces ADD COLUMN IF NOT EXISTS distribution TEXT NOT NULL DEFAULT 'public';

		CREATE TABLE IF NOT EXISTS _system.space_installs (
			space_id TEXT NOT NULL,
			org_id TEXT NOT NULL,
			installed_at TIMESTAMPTZ DEFAULT now(),
			PRIMARY KEY (space_id, org_id)
		);

		CREATE TABLE IF NOT EXISTS _system.space_allowlist (
			space_id TEXT NOT NULL,
			org_id TEXT NOT NULL,
			added_at TIMESTAMPTZ DEFAULT now(),
			PRIMARY KEY (space_id, org_id)
		);

		CREATE TABLE IF NOT EXISTS _system.manifests (
			id SERIAL PRIMARY KEY,
			space_id TEXT NOT NULL REFERENCES _system.spaces(id),
			version TEXT NOT NULL,
			manifest JSONB NOT NULL,
			created_at TIMESTAMPTZ DEFAULT now(),
			UNIQUE(space_id, version)
		);

		CREATE TABLE IF NOT EXISTS _system.provisions (
			id SERIAL PRIMARY KEY,
			space_id TEXT NOT NULL,
			project_id TEXT NOT NULL,
			schema_name TEXT NOT NULL,
			manifest_version TEXT NOT NULL,
			provisioned_at TIMESTAMPTZ DEFAULT now(),
			UNIQUE(space_id, project_id)
		);

		CREATE TABLE IF NOT EXISTS _system.graph_events (
			id BIGSERIAL PRIMARY KEY,
			schema_name TEXT NOT NULL,
			table_name TEXT NOT NULL,
			record_id TEXT NOT NULL,
			action TEXT NOT NULL,
			payload JSONB,
			previous_payload JSONB,
			created_at TIMESTAMPTZ DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS idx_graph_events_stream ON _system.graph_events(schema_name, table_name, id);
	`).Error
}

func (r *Registry) initSystemSQLite() error {
	// SQLite: no schemas, use prefix _system__
	err := r.db.Exec(`
		CREATE TABLE IF NOT EXISTS _system__spaces (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			latest_version TEXT NOT NULL DEFAULT '0.0.0',
			owner_user_id TEXT,
			registered_at DATETIME DEFAULT (datetime('now'))
		)
	`).Error
	if err != nil {
		return err
	}

	// Backfill: add owner_user_id / bundle_id / publisher_org_id columns if
	// table existed before this migration. SQLite has no IF NOT EXISTS on
	// ALTER TABLE, so ignore the error when the column already exists.
	r.db.Exec(`ALTER TABLE _system__spaces ADD COLUMN owner_user_id TEXT`)
	r.db.Exec(`ALTER TABLE _system__spaces ADD COLUMN bundle_id TEXT`)
	r.db.Exec(`ALTER TABLE _system__spaces ADD COLUMN publisher_org_id TEXT`)

	err = r.db.Exec(`
		CREATE TABLE IF NOT EXISTS _system__space_bundles (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			owner_org_id TEXT NOT NULL,
			created_at DATETIME DEFAULT (datetime('now'))
		)
	`).Error
	if err != nil {
		return err
	}

	r.db.Exec(`CREATE INDEX IF NOT EXISTS idx_spaces_bundle_id ON _system__spaces(bundle_id)`)
	r.db.Exec(`CREATE INDEX IF NOT EXISTS idx_space_bundles_owner_org_id ON _system__space_bundles(owner_org_id)`)

	err = r.db.Exec(`
		CREATE TABLE IF NOT EXISTS _system__space_imports (
			space_id TEXT NOT NULL,
			from_space_id TEXT NOT NULL,
			model_name TEXT NOT NULL,
			created_at DATETIME DEFAULT (datetime('now')),
			PRIMARY KEY (space_id, model_name)
		)
	`).Error
	if err != nil {
		return err
	}
	r.db.Exec(`CREATE INDEX IF NOT EXISTS idx_space_imports_from ON _system__space_imports(from_space_id)`)

	// distribution column — added as nullable because SQLite ALTER TABLE can't
	// do "NOT NULL with DEFAULT" on existing tables. We coerce empty string to
	// "public" at read time (see GetSpaceDistribution).
	r.db.Exec(`ALTER TABLE _system__spaces ADD COLUMN distribution TEXT DEFAULT 'public'`)

	err = r.db.Exec(`
		CREATE TABLE IF NOT EXISTS _system__space_installs (
			space_id TEXT NOT NULL,
			org_id TEXT NOT NULL,
			installed_at DATETIME DEFAULT (datetime('now')),
			PRIMARY KEY (space_id, org_id)
		)
	`).Error
	if err != nil {
		return err
	}

	err = r.db.Exec(`
		CREATE TABLE IF NOT EXISTS _system__space_allowlist (
			space_id TEXT NOT NULL,
			org_id TEXT NOT NULL,
			added_at DATETIME DEFAULT (datetime('now')),
			PRIMARY KEY (space_id, org_id)
		)
	`).Error
	if err != nil {
		return err
	}

	err = r.db.Exec(`
		CREATE TABLE IF NOT EXISTS _system__manifests (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			space_id TEXT NOT NULL REFERENCES _system__spaces(id),
			version TEXT NOT NULL,
			manifest TEXT NOT NULL,
			created_at DATETIME DEFAULT (datetime('now')),
			UNIQUE(space_id, version)
		)
	`).Error
	if err != nil {
		return err
	}

	err = r.db.Exec(`
		CREATE TABLE IF NOT EXISTS _system__provisions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			space_id TEXT NOT NULL,
			project_id TEXT NOT NULL,
			schema_name TEXT NOT NULL,
			manifest_version TEXT NOT NULL,
			provisioned_at DATETIME DEFAULT (datetime('now')),
			UNIQUE(space_id, project_id)
		)
	`).Error
	if err != nil {
		return err
	}

	err = r.db.Exec(`
		CREATE TABLE IF NOT EXISTS _system__graph_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			schema_name TEXT NOT NULL,
			table_name TEXT NOT NULL,
			record_id TEXT NOT NULL,
			action TEXT NOT NULL,
			payload TEXT,
			previous_payload TEXT,
			created_at DATETIME DEFAULT (datetime('now'))
		)
	`).Error
	if err != nil {
		return err
	}
	return r.db.Exec(`CREATE INDEX IF NOT EXISTS idx_graph_events_stream ON _system__graph_events(schema_name, table_name, id)`).Error
}

// upsert executes an INSERT ... ON CONFLICT DO UPDATE that works on both PG and SQLite.
// conflictCol is the column(s) for ON CONFLICT, updateCols are SET assignments.
func (r *Registry) upsert(table string, cols []string, args []any, conflictCol string, updateCols []string) error {
	placeholders := make([]string, len(cols))
	for i := range placeholders {
		placeholders[i] = "?"
	}
	sets := make([]string, len(updateCols))
	for i, c := range updateCols {
		sets[i] = fmt.Sprintf("%s = excluded.%s", c, c)
	}
	sql := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (%s) DO UPDATE SET %s",
		table, strings.Join(cols, ","), strings.Join(placeholders, ","),
		conflictCol, strings.Join(sets, ", "))
	return r.db.Exec(sql, args...).Error
}

// GetSpaceScopes returns the set of supported scopes for a space from
// its latest manifest. Reads the plural `scopes` field; falls back to
// the legacy singular `scope` for spaces still on the old SDK. Empty
// result means the space declared no scope (legacy/project shared).
func (r *Registry) GetSpaceScopes(spaceID string) []string {
	models, err := r.GetModels(spaceID)
	if err != nil || len(models) == 0 {
		return nil
	}
	for _, m := range models {
		if m.Options == nil {
			continue
		}
		if len(m.Options.Scopes) > 0 {
			return m.Options.Scopes
		}
		if m.Options.Scope != "" {
			return []string{m.Options.Scope}
		}
	}
	return nil
}

// GetSpaceScope returns a single scope for a space, picking from the
// supported scopes given the caller's org context. A space that declares
// both "app" and "org" resolves to "org" when the caller has an org, and
// to "app" otherwise — so personal callers always land in their own
// per-user partition even on org-aware spaces.
func (r *Registry) GetSpaceScope(spaceID, callerOrgID string) string {
	scopes := r.GetSpaceScopes(spaceID)
	if len(scopes) == 0 {
		return ""
	}
	has := func(s string) bool {
		for _, v := range scopes {
			if v == s {
				return true
			}
		}
		return false
	}
	if callerOrgID != "" && has("org") {
		return "org"
	}
	if has("app") {
		return "app"
	}
	return scopes[0]
}

// CreateSpaceBundle inserts a new bundle owned by the given org. Returns an
// error if a bundle with that id already exists.
func (r *Registry) CreateSpaceBundle(id, name, ownerOrgID string) error {
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	ownerOrgID = strings.TrimSpace(ownerOrgID)
	if id == "" || name == "" || ownerOrgID == "" {
		return fmt.Errorf("id, name, owner_org_id required")
	}
	// Check for existing to give a clean error instead of a constraint violation.
	existing, _ := r.getBundleOwnerOrg(id)
	if existing != "" {
		return fmt.Errorf("bundle %q already exists", id)
	}
	return r.db.Exec(
		fmt.Sprintf("INSERT INTO %s (id, name, owner_org_id) VALUES (?, ?, ?)", systemTable("space_bundles")),
		id, name, ownerOrgID,
	).Error
}

// GetSpaceBundle returns a single bundle by id.
func (r *Registry) GetSpaceBundle(id string) (*SpaceBundle, error) {
	rows, err := r.db.Raw(
		fmt.Sprintf("SELECT id, name, owner_org_id FROM %s WHERE id = ?", systemTable("space_bundles")),
		strings.TrimSpace(id),
	).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, nil
	}
	var b SpaceBundle
	if err := rows.Scan(&b.ID, &b.Name, &b.OwnerOrgID); err != nil {
		return nil, err
	}
	return &b, nil
}

// ListSpaceBundlesByOwner returns bundles owned by the given org.
func (r *Registry) ListSpaceBundlesByOwner(ownerOrgID string) ([]SpaceBundle, error) {
	rows, err := r.db.Raw(
		fmt.Sprintf("SELECT id, name, owner_org_id FROM %s WHERE owner_org_id = ? ORDER BY created_at DESC", systemTable("space_bundles")),
		strings.TrimSpace(ownerOrgID),
	).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SpaceBundle
	for rows.Next() {
		var b SpaceBundle
		if err := rows.Scan(&b.ID, &b.Name, &b.OwnerOrgID); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// getBundleOwnerOrg returns "" if the bundle does not exist.
func (r *Registry) getBundleOwnerOrg(bundleID string) (string, error) {
	var owner string
	err := r.db.Raw(
		fmt.Sprintf("SELECT COALESCE(owner_org_id, '') FROM %s WHERE id = ?", systemTable("space_bundles")),
		bundleID,
	).Scan(&owner).Error
	return owner, err
}

// getSpacePublisherOrg returns "" if the space is new or has no publisher org recorded.
func (r *Registry) getSpacePublisherOrg(spaceID string) (string, error) {
	var org string
	err := r.db.Raw(
		fmt.Sprintf("SELECT COALESCE(publisher_org_id, '') FROM %s WHERE id = ?", systemTable("spaces")),
		spaceID,
	).Scan(&org).Error
	return org, err
}

func (r *Registry) getSpaceOwnerUser(spaceID string) (string, error) {
	var user string
	err := r.db.Raw(
		fmt.Sprintf("SELECT COALESCE(owner_user_id, '') FROM %s WHERE id = ?", systemTable("spaces")),
		spaceID,
	).Scan(&user).Error
	return user, err
}

// GetSpaceOwnerUser is the exported accessor used to enforce ownership for personal publishers.
func (r *Registry) GetSpaceOwnerUser(spaceID string) string {
	user, _ := r.getSpaceOwnerUser(spaceID)
	return user
}

// GetSpacePublisherOrg is the exported accessor used by the graphql layer to
// enforce publisher_admin access. Returns "" if the space has no publisher org
// recorded (legacy standalone space).
func (r *Registry) GetSpacePublisherOrg(spaceID string) string {
	org, _ := r.getSpacePublisherOrg(spaceID)
	return org
}

// getSpaceBundleID returns "" if the space is new or is standalone.
func (r *Registry) getSpaceBundleID(spaceID string) (string, error) {
	var bid string
	err := r.db.Raw(
		fmt.Sprintf("SELECT COALESCE(bundle_id, '') FROM %s WHERE id = ?", systemTable("spaces")),
		spaceID,
	).Scan(&bid).Error
	return bid, err
}

// GetSpaceBundleIDFor is the exported accessor for the graphql layer.
// Returns "" for legacy standalone spaces.
func (r *Registry) GetSpaceBundleIDFor(spaceID string) string {
	bid, _ := r.getSpaceBundleID(spaceID)
	return bid
}

// replaceSpaceImports validates each ImportSpec against the publisher grouping
// and persists the resolved (space_id, from_space_id, model_name) rows. Any
// prior imports for the space are deleted first — manifest is the source of
// truth, so dropping an import from the manifest removes it at runtime too.
func (r *Registry) replaceSpaceImports(spaceID, bundleID string, imports []ImportSpec) error {
	for _, imp := range imports {
		from := strings.TrimSpace(imp.From)
		if from == "" {
			return fmt.Errorf("import missing 'from'")
		}
		if from == spaceID {
			return fmt.Errorf("import cannot reference self (%q)", spaceID)
		}
		srcBundle, err := r.getSpaceBundleID(from)
		if err != nil {
			return fmt.Errorf("lookup source space %q: %w", from, err)
		}
		if srcBundle == "" {
			return fmt.Errorf("source space %q does not belong to any bundle", from)
		}
		if srcBundle != bundleID {
			return fmt.Errorf("source space %q is in a different bundle", from)
		}
		if len(imp.Models) == 0 {
			return fmt.Errorf("import from %q must list at least one model", from)
		}
		srcModels, err := r.GetModels(from)
		if err != nil {
			return fmt.Errorf("load source manifest for %q: %w", from, err)
		}
		srcModelSet := make(map[string]bool, len(srcModels))
		for _, m := range srcModels {
			srcModelSet[m.Name] = true
		}
		for _, mname := range imp.Models {
			if !srcModelSet[mname] {
				return fmt.Errorf("source space %q does not export model %q", from, mname)
			}
		}
	}

	// Wipe then insert — imports are small per space, simple replace semantics
	// beat merging partial state.
	if err := r.db.Exec(
		fmt.Sprintf("DELETE FROM %s WHERE space_id = ?", systemTable("space_imports")),
		spaceID,
	).Error; err != nil {
		return err
	}
	for _, imp := range imports {
		for _, mname := range imp.Models {
			if err := r.db.Exec(
				fmt.Sprintf("INSERT INTO %s (space_id, from_space_id, model_name) VALUES (?, ?, ?)", systemTable("space_imports")),
				spaceID, strings.TrimSpace(imp.From), mname,
			).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

// ResolvedModel identifies where a model's data lives and which space's rules
// govern access. SourceSpaceID is where the tables are; AccessSpaceID is whose
// manifest defines the access rules for this resolution. For local models
// both are the same; for imports SourceSpaceID is the exporting space and
// AccessSpaceID is the importing space (so the importer can override rules
// like `publisher_admin` without touching the source manifest).
type ResolvedModel struct {
	SourceSpaceID string
	AccessSpaceID string
	Model         ModelDef
	Imported      bool
}

// ResolveModel looks up a model by name from a space, falling back to
// imports when the model isn't defined locally. Returns nil when not found.
func (r *Registry) ResolveModel(spaceID, modelName string) *ResolvedModel {
	localModels, _ := r.GetModels(spaceID)
	for _, m := range localModels {
		if m.Name == modelName {
			return &ResolvedModel{
				SourceSpaceID: spaceID,
				AccessSpaceID: spaceID,
				Model:         m,
				Imported:      false,
			}
		}
	}

	// Import fallback — look up the source space for this model.
	var fromSpace string
	err := r.db.Raw(
		fmt.Sprintf("SELECT from_space_id FROM %s WHERE space_id = ? AND model_name = ?", systemTable("space_imports")),
		spaceID, modelName,
	).Scan(&fromSpace).Error
	if err != nil || fromSpace == "" {
		return nil
	}
	srcModels, _ := r.GetModels(fromSpace)
	for _, m := range srcModels {
		if m.Name == modelName {
			return &ResolvedModel{
				SourceSpaceID: fromSpace,
				AccessSpaceID: spaceID,
				Model:         m,
				Imported:      true,
			}
		}
	}
	return nil
}

// GetModelAccess returns the access rules for a model in a space
func (r *Registry) GetModelAccess(spaceID, modelName string) *AccessRules {
	models, err := r.GetModels(spaceID)
	if err != nil {
		return nil
	}
	for _, m := range models {
		if m.Name == modelName && m.Options != nil {
			return m.Options.Access
		}
	}
	return nil
}

// GetModelFields returns field definitions for a model in a space
func (r *Registry) GetModelFields(spaceID, modelName string) []FieldDef {
	models, err := r.GetModels(spaceID)
	if err != nil {
		return nil
	}
	for _, m := range models {
		if m.Name == modelName {
			return m.Fields
		}
	}
	return nil
}

// GetRelationFields returns only relation fields for a model
func (r *Registry) GetRelationFields(spaceID, modelName string) []FieldDef {
	fields := r.GetModelFields(spaceID, modelName)
	var rels []FieldDef
	for _, f := range fields {
		if f.Type == "relation" {
			rels = append(rels, f)
		}
	}
	return rels
}

// Register processes a manifest and creates/migrates the schema.
// callerUserID and callerOrgID identify the publisher (from accounts).
// manifest.ProductID, if set, must belong to an existing product owned by
// callerOrgID — cross-org product hijacking is rejected.
func (r *Registry) Register(spaceID, spaceName, version string, manifest Manifest, projectID string, callerUserID ...string) error {
	return r.RegisterWithOrg(spaceID, spaceName, version, manifest, projectID, "", callerUserID...)
}

// RegisterWithOrg is Register + caller org context (preferred new entry point).
// callerOrgID becomes the space's publisher_org_id on first registration.
func (r *Registry) RegisterWithOrg(spaceID, spaceName, version string, manifest Manifest, projectID, callerOrgID string, callerUserID ...string) error {
	ownerID := ""
	if len(callerUserID) > 0 {
		ownerID = callerUserID[0]
	}

	// Bundle ownership check: if the manifest claims a bundle_id, it must
	// exist and be owned by the caller's org. Prevents a dev from attaching
	// their space to another publisher's bundle.
	bundleID := strings.TrimSpace(manifest.BundleID)
	if bundleID != "" {
		if callerOrgID == "" {
			return fmt.Errorf("bundle_id requires authenticated org context")
		}
		ownerOrg, err := r.getBundleOwnerOrg(bundleID)
		if err != nil {
			return fmt.Errorf("lookup bundle %q: %w", bundleID, err)
		}
		if ownerOrg == "" {
			return fmt.Errorf("bundle %q does not exist", bundleID)
		}
		if ownerOrg != callerOrgID {
			return fmt.Errorf("bundle %q is owned by a different org", bundleID)
		}
	}

	// Also reject attaching to a space already owned by another org.
	existingPublisher, _ := r.getSpacePublisherOrg(spaceID)
	if existingPublisher != "" && callerOrgID != "" && existingPublisher != callerOrgID {
		return fmt.Errorf("space %q is published by a different org", spaceID)
	}

	cols := []string{"id", "name", "latest_version"}
	args := []any{spaceID, spaceName, version}
	updates := []string{"latest_version"}
	if ownerID != "" {
		cols = append(cols, "owner_user_id")
		args = append(args, ownerID)
	}
	if callerOrgID != "" {
		// Always refresh publisher_org_id on conflict so spaces created before
		// this column existed get backfilled on the next publish. The cross-org
		// mismatch check above already rejects an attempt to move a space to a
		// different org, so the only case that reaches here with a different
		// existing value is the legacy NULL → callerOrgID backfill we want.
		cols = append(cols, "publisher_org_id")
		args = append(args, callerOrgID)
		updates = append(updates, "publisher_org_id")
	}
	if bundleID != "" {
		cols = append(cols, "bundle_id")
		args = append(args, bundleID)
		updates = append(updates, "bundle_id")
	}
	if err := r.upsert(systemTable("spaces"), cols, args, "id", updates); err != nil {
		return fmt.Errorf("register space: %w", err)
	}

	// Store manifest
	manifestJSON, _ := json.Marshal(manifest)
	if err := r.upsert(systemTable("manifests"),
		[]string{"space_id", "version", "manifest"}, []any{spaceID, version, string(manifestJSON)},
		"space_id, version", []string{"manifest"}); err != nil {
		return fmt.Errorf("store manifest: %w", err)
	}

	// Validate + persist cross-space imports. Each import source must share
	// this space's bundle_id (same publisher). Cross-bundle imports are
	// rejected — the source space belongs to a different bundle.
	if len(manifest.Imports) > 0 {
		if bundleID == "" {
			return fmt.Errorf("imports require a bundle_id on this space")
		}
		if err := r.replaceSpaceImports(spaceID, bundleID, manifest.Imports); err != nil {
			return fmt.Errorf("imports: %w", err)
		}
	} else {
		// Clear any stale imports if a later manifest removed them.
		if err := r.db.Exec(
			fmt.Sprintf("DELETE FROM %s WHERE space_id = ?", systemTable("space_imports")),
			spaceID,
		).Error; err != nil {
			return fmt.Errorf("clear imports: %w", err)
		}
	}

	// Create schema (PostgreSQL only — SQLite uses table prefixes)
	schemaName := SchemaName(spaceID, projectID)
	if err := r.createSchemaIfNotExistsSafe(schemaName); err != nil {
		return err
	}

	// Create/migrate tables
	for _, model := range manifest.Models {
		if err := r.ensureTable(context.Background(), schemaName, model); err != nil {
			return fmt.Errorf("ensure table %s.%s: %w", schemaName, model.Name, err)
		}
	}

	// Record provision
	if err := r.upsert(systemTable("provisions"),
		[]string{"space_id", "project_id", "schema_name", "manifest_version"},
		[]any{spaceID, projectID, schemaName, version},
		"space_id, project_id", []string{"manifest_version"}); err != nil {
		return err
	}

	return nil
}

// ensureTable creates a table if not exists, and uses GORM migrator to
// add any missing columns (additive-only migration).
func (r *Registry) ensureTable(_ context.Context, schemaName string, model ModelDef) error {
	// Raw (unquoted) form is what the GORM migrator wants for HasTable +
	// what the information_schema lookup in hasColumn needs; quoted form
	// is for SQL building only (CREATE TABLE / ALTER TABLE emit their own
	// quoting).
	tableRaw := engine.TableRefRaw(schemaName, sanitize(model.Name))
	tableName := engine.TableRef(schemaName, sanitize(model.Name))
	migrator := r.db.Migrator()

	if !migrator.HasTable(tableRaw) {
		// Build full CREATE TABLE with all columns
		var cols []string
		cols = append(cols, IDColumn())

		for _, f := range model.Fields {
			col := r.fieldToColumn(f, schemaName)
			if col != "" {
				cols = append(cols, col)
			}
		}

		tsType := TimestampType()
		nowDef := NowDefault()
		cols = append(cols,
			fmt.Sprintf(`"created_at" %s %s`, tsType, nowDef),
			fmt.Sprintf(`"updated_at" %s %s`, tsType, nowDef),
			`"created_by" TEXT`,
		)

		// IF NOT EXISTS + advisory lock: two parallel model loads for a fresh
		// tenant schema would otherwise both pass HasTable() above and race on
		// CREATE, surfacing as "duplicate key value violates unique constraint".
		createSQL := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n  %s\n)", tableName, strings.Join(cols, ",\n  "))
		if err := r.createTableIfNotExistsSafe(schemaName, sanitize(model.Name), createSQL); err != nil {
			return fmt.Errorf("create table: %w", err)
		}
	} else {
		// Table exists — add missing columns via GORM migrator
		for _, f := range model.Fields {
			if f.Type == "relation" && f.Relation == "hasMany" {
				continue
			}
			colName := sanitize(f.Name)
			if f.Type == "relation" {
				colName += "_id"
			}
			if !r.hasColumn(tableRaw, colName) {
				sqlType := FieldTypeToSQL(f)
				r.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", tableName, quoteIdent(colName), sqlType))
			}
		}
	}

	// Create indexes
	for _, f := range model.Fields {
		if f.Index || f.Type == "relation" {
			colName := sanitize(f.Name)
			if f.Type == "relation" {
				colName += "_id"
			}
			idxName := fmt.Sprintf("idx_%s_%s", sanitize(model.Name), colName)
			r.db.Exec(fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (%s)", idxName, tableName, quoteIdent(colName)))
		}
	}

	return nil
}

// hasColumn checks if a column exists in a table (works for both PG and SQLite).
func (r *Registry) hasColumn(table, column string) bool {
	var count int
	if engine.IsPostgres() {
		// Split schema.table for information_schema query
		parts := strings.SplitN(table, ".", 2)
		if len(parts) == 2 {
			r.db.Raw("SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = ? AND table_name = ? AND column_name = ?",
				parts[0], parts[1], column).Scan(&count)
		} else {
			r.db.Raw("SELECT COUNT(*) FROM information_schema.columns WHERE table_name = ? AND column_name = ?",
				table, column).Scan(&count)
		}
	} else {
		r.db.Raw(fmt.Sprintf("SELECT COUNT(*) FROM pragma_table_info('%s') WHERE name = ?", table), column).Scan(&count)
	}
	return count > 0
}

func (r *Registry) fieldToColumn(f FieldDef, schemaName string) string {
	if f.Type == "relation" && f.Relation == "hasMany" {
		return "" // virtual relation, no column
	}

	colName := sanitize(f.Name)
	sqlType := FieldTypeToSQL(f)

	if f.Type == "relation" {
		colName = colName + "_id"
		relType := FieldTypeToSQL(f) // UUID for PG, TEXT for SQLite
		return fmt.Sprintf("%s %s", quoteIdent(colName), relType)
	}

	parts := []string{quoteIdent(colName), sqlType}
	if f.Required {
		parts = append(parts, "NOT NULL")
	}
	if f.Unique {
		parts = append(parts, "UNIQUE")
	}
	if f.Default != nil {
		switch v := f.Default.(type) {
		case bool:
			if engine.IsPostgres() {
				if v {
					parts = append(parts, "DEFAULT true")
				} else {
					parts = append(parts, "DEFAULT false")
				}
			} else {
				if v {
					parts = append(parts, "DEFAULT 1")
				} else {
					parts = append(parts, "DEFAULT 0")
				}
			}
		case string:
			parts = append(parts, "DEFAULT "+sqlStringLiteral(v))
		case float64:
			parts = append(parts, fmt.Sprintf("DEFAULT %v", v))
		}
	}
	if f.Type == "enum" && len(f.Values) > 0 {
		quoted := make([]string, len(f.Values))
		for i, v := range f.Values {
			quoted[i] = fmt.Sprintf("'%s'", v)
		}
		parts = append(parts, fmt.Sprintf("CHECK (%s IN (%s))", quoteIdent(colName), strings.Join(quoted, ",")))
	}

	return strings.Join(parts, " ")
}

func sqlStringLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// GetModels returns models for a space
func (r *Registry) GetModels(spaceID string) ([]ModelDef, error) {
	manifestsTable := systemTable("manifests")

	var manifestJSON string
	err := r.db.Raw(fmt.Sprintf(`
		SELECT manifest FROM %s
		WHERE space_id = ?
		ORDER BY created_at DESC LIMIT 1
	`, manifestsTable), spaceID).Scan(&manifestJSON).Error
	if err != nil {
		return nil, err
	}
	var m Manifest
	json.Unmarshal([]byte(manifestJSON), &m)
	return m.Models, nil
}

// GetSchemaName returns the database schema name for a space+project
func (r *Registry) GetSchemaName(spaceID, projectID string) (string, error) {
	provisionsTable := systemTable("provisions")

	var name string
	err := r.db.Raw(fmt.Sprintf(`
		SELECT schema_name FROM %s
		WHERE space_id = ? AND project_id = ?
	`, provisionsTable), spaceID, projectID).Scan(&name).Error
	if err != nil {
		return "", fmt.Errorf("no provision for space=%s project=%s", spaceID, projectID)
	}
	if name == "" {
		return "", fmt.Errorf("no provision for space=%s project=%s", spaceID, projectID)
	}
	return name, nil
}

// EnsureTenantSchema lazily provisions the tables for a tenant partition
// (per-org or per-user). Org/app-scoped spaces don't know the tenant id at
// publish time, so the GraphQL handler calls this on the request path the
// first time a tenant queries the space — the second call is a no-op
// because every CREATE uses IF NOT EXISTS.
//
// Without this, every personal/org user would hit "relation does not exist"
// the first time they touch an org-scoped space.
func (r *Registry) EnsureTenantSchema(spaceID, schemaName string) error {
	models, err := r.GetModels(spaceID)
	if err != nil {
		return fmt.Errorf("get models for %s: %w", spaceID, err)
	}
	if len(models) == 0 {
		return fmt.Errorf("no models registered for space %s", spaceID)
	}
	if err := r.createSchemaIfNotExistsSafe(schemaName); err != nil {
		return err
	}
	for _, model := range models {
		if err := r.ensureTable(context.Background(), schemaName, model); err != nil {
			return fmt.Errorf("ensure table %s.%s: %w", schemaName, model.Name, err)
		}
	}
	return nil
}

// HandleRegister is the HTTP handler for schema registration
func (r *Registry) HandleRegister(w http.ResponseWriter, req *http.Request) {
	var body struct {
		SpaceID   string   `json:"space_id"`
		SpaceName string   `json:"space_name"`
		Version   string   `json:"version"`
		ProjectID string   `json:"project_id"`
		Manifest  Manifest `json:"manifest"`
	}

	data, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, `{"error":"bad request"}`, 400)
		return
	}
	if err := json.Unmarshal(data, &body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, 400)
		return
	}

	// Developer enrollment requirement removed — developer portal is gone,
	// my.lisaos.dev gateway is the single entrypoint.

	// Require user identity for ownership verification
	callerUserID := req.Header.Get("X-Auth-User-ID")
	if callerUserID == "" {
		http.Error(w, `{"error":"X-Auth-User-ID header required"}`, http.StatusForbidden)
		return
	}
	// Org context (optional; required only if the manifest claims a product_id).
	// Populated by auth middleware from accounts /me/scope when Scope == "org".
	callerOrgID := req.Header.Get("X-Auth-Org-ID")

	if body.SpaceID == "" || body.ProjectID == "" {
		http.Error(w, `{"error":"space_id and project_id required"}`, 400)
		return
	}
	if body.SpaceName == "" {
		body.SpaceName = body.SpaceID
	}
	if body.Version == "" {
		body.Version = "0.0.1"
	}

	// Ownership check: if this space already exists, verify the caller has
	// rights to publish over it. Rights come from either:
	//   1. Same user as the original owner_user_id (personal publishers), OR
	//   2. Same org as the space's publisher_org_id (org-scoped publishers).
	//
	// The org branch is what lets an org member re-publish a space that
	// another member created — critical once spaces belong to bundles owned
	// by an org rather than a single person. Both branches still require
	// the auth middleware to have attested the caller's identity (this
	// handler reads X-Auth-User-ID / X-Auth-Org-ID which are stripped +
	// re-written by RequireAuth from the bearer token).
	spacesTable := systemTable("spaces")
	var existingOwner, existingPublisher string
	_ = r.db.Raw(
		fmt.Sprintf("SELECT COALESCE(owner_user_id, ''), COALESCE(publisher_org_id, '') FROM %s WHERE id = ?", spacesTable),
		body.SpaceID,
	).Row().Scan(&existingOwner, &existingPublisher)

	sameUser := existingOwner != "" && existingOwner == callerUserID
	sameOrg := existingPublisher != "" && callerOrgID != "" && existingPublisher == callerOrgID
	// Legacy claim: spaces registered before publisher_org_id existed have it
	// empty in the DB. Allow any authenticated org caller to claim them on the
	// next push; RegisterWithOrg below backfills publisher_org_id so subsequent
	// pushes go through the strict sameOrg branch. This matches the backfill
	// intent documented in RegisterWithOrg.
	legacyOrgClaim := existingPublisher == "" && callerOrgID != ""
	if existingOwner != "" && !sameUser && !sameOrg && !legacyOrgClaim {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(403)
		json.NewEncoder(w).Encode(map[string]any{
			"error":            "You are not the owner of this space. Fork to a new space_id to publish your own version.",
			"owner_user_id":    existingOwner,
			"publisher_org_id": existingPublisher,
		})
		return
	}

	if err := r.RegisterWithOrg(body.SpaceID, body.SpaceName, body.Version, body.Manifest, body.ProjectID, callerOrgID, callerUserID); err != nil {
		// Map caller-facing validation errors to 400/403 rather than 500.
		msg := err.Error()
		status := 500
		switch {
		case strings.Contains(msg, "does not exist"),
			strings.Contains(msg, "requires authenticated org context"):
			status = 400
		case strings.Contains(msg, "owned by a different org"),
			strings.Contains(msg, "published by a different org"):
			status = 403
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{"error": msg})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":          true,
		"schema_name": SchemaName(body.SpaceID, body.ProjectID),
		"tables":      len(body.Manifest.Models),
		"registered":  time.Now().UTC().Format(time.RFC3339),
	})
}

// HandleGetSchema returns schema info for a space
func (r *Registry) HandleGetSchema(w http.ResponseWriter, req *http.Request) {
	spaceID := req.PathValue("spaceId")
	models, err := r.GetModels(spaceID)
	if err != nil {
		http.Error(w, `{"error":"space not found"}`, 404)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"space_id": spaceID,
		"models":   models,
	})
}
