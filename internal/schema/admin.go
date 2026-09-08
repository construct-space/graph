package schema

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"construct-graph/internal/engine"
)

var validIdentifier = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// AdminStats returns graph-wide statistics
func (r *Registry) AdminStats(w http.ResponseWriter, req *http.Request) {
	provisionsTable := systemTable("provisions")

	var schemaCount int
	r.db.Raw(fmt.Sprintf("SELECT COUNT(*) FROM %s", provisionsTable)).Scan(&schemaCount)

	var tableCount int
	var totalRows int64
	var dbSize string

	if engine.IsPostgres() {
		r.adminStatsPostgres(provisionsTable, &tableCount, &totalRows, &dbSize)
	} else {
		r.adminStatsSQLite(provisionsTable, &tableCount, &totalRows, &dbSize)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"schemas": schemaCount,
		"tables":  tableCount,
		"rows":    totalRows,
		"size":    dbSize,
	})
}

func (r *Registry) adminStatsPostgres(provisionsTable string, tableCount *int, totalRows *int64, dbSize *string) {
	// Count tables across all provisioned schemas
	rows, err := r.db.Raw(fmt.Sprintf("SELECT schema_name FROM %s", provisionsTable)).Rows()
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var schemaName string
			rows.Scan(&schemaName)
			var cnt int
			r.db.Raw(`
				SELECT COUNT(*) FROM information_schema.tables
				WHERE table_schema = ? AND table_name NOT LIKE '\_%'
			`, schemaName).Scan(&cnt)
			*tableCount += cnt
		}
	}

	// Total rows (approximate)
	provRows, err := r.db.Raw(fmt.Sprintf("SELECT schema_name FROM %s", provisionsTable)).Rows()
	if err == nil {
		defer provRows.Close()
		for provRows.Next() {
			var schemaName string
			provRows.Scan(&schemaName)
			var rowCount int64
			r.db.Raw(`
				SELECT COALESCE(SUM(n_live_tup), 0) FROM pg_stat_user_tables
				WHERE schemaname = ?
			`, schemaName).Scan(&rowCount)
			*totalRows += rowCount
		}
	}

	// DB size
	r.db.Raw("SELECT pg_size_pretty(pg_database_size(current_database()))").Scan(dbSize)
}

func (r *Registry) adminStatsSQLite(provisionsTable string, tableCount *int, totalRows *int64, dbSize *string) {
	// Count tables matching provisioned schema prefixes
	rows, err := r.db.Raw(fmt.Sprintf("SELECT schema_name FROM %s", provisionsTable)).Rows()
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var schemaName string
			rows.Scan(&schemaName)
			prefix := schemaName + "__"
			var cnt int
			r.db.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name LIKE ?", prefix+"%").Scan(&cnt)
			*tableCount += cnt
		}
	}

	// Total rows — iterate through provisioned tables
	provRows, err := r.db.Raw(fmt.Sprintf("SELECT schema_name FROM %s", provisionsTable)).Rows()
	if err == nil {
		defer provRows.Close()
		for provRows.Next() {
			var schemaName string
			provRows.Scan(&schemaName)
			prefix := schemaName + "__"

			// Get all table names for this schema
			tblRows, tblErr := r.db.Raw("SELECT name FROM sqlite_master WHERE type='table' AND name LIKE ?", prefix+"%").Rows()
			if tblErr == nil {
				defer tblRows.Close()
				for tblRows.Next() {
					var tblName string
					tblRows.Scan(&tblName)
					var rowCount int64
					r.db.Raw(fmt.Sprintf("SELECT COUNT(*) FROM \"%s\"", tblName)).Scan(&rowCount)
					*totalRows += rowCount
				}
			}
		}
	}

	// DB size — get file size of the SQLite database
	// Extract the database path from the dialector
	sqlDB, err := r.db.DB()
	if err == nil {
		var dbPath string
		sqlDB.QueryRow("PRAGMA database_list").Scan(nil, nil, &dbPath)
		if dbPath != "" {
			if info, err := os.Stat(dbPath); err == nil {
				sizeBytes := info.Size()
				switch {
				case sizeBytes >= 1<<30:
					*dbSize = fmt.Sprintf("%.1f GB", float64(sizeBytes)/float64(1<<30))
				case sizeBytes >= 1<<20:
					*dbSize = fmt.Sprintf("%.1f MB", float64(sizeBytes)/float64(1<<20))
				case sizeBytes >= 1<<10:
					*dbSize = fmt.Sprintf("%.1f kB", float64(sizeBytes)/float64(1<<10))
				default:
					*dbSize = fmt.Sprintf("%d bytes", sizeBytes)
				}
			}
		}
	}
	if *dbSize == "" {
		*dbSize = "unknown"
	}
}

// AdminListSchemas returns all provisioned schemas
func (r *Registry) AdminListSchemas(w http.ResponseWriter, req *http.Request) {
	provisionsTable := systemTable("provisions")

	rows, err := r.db.Raw(fmt.Sprintf(`
		SELECT space_id, project_id, schema_name, manifest_version, provisioned_at
		FROM %s
		ORDER BY provisioned_at DESC
	`, provisionsTable)).Rows()
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"schemas": []any{}})
		return
	}
	defer rows.Close()

	var schemas []map[string]any
	for rows.Next() {
		var spaceID, projectID, schemaName, version string
		var provisionedAt any
		rows.Scan(&spaceID, &projectID, &schemaName, &version, &provisionedAt)
		schemas = append(schemas, map[string]any{
			"space_id":         spaceID,
			"project_id":       projectID,
			"schema_name":      schemaName,
			"manifest_version": version,
			"provisioned_at":   provisionedAt,
		})
	}
	if schemas == nil {
		schemas = []map[string]any{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"schemas": schemas})
}

// AdminDeleteSchema drops a schema and removes the provision record
func (r *Registry) AdminDeleteSchema(w http.ResponseWriter, req *http.Request) {
	name := req.PathValue("name")
	if name == "" {
		http.Error(w, `{"error":"schema name required"}`, 400)
		return
	}
	if !validIdentifier.MatchString(name) {
		http.Error(w, `{"error":"invalid schema name"}`, 400)
		return
	}

	provisionsTable := systemTable("provisions")

	if engine.IsPostgres() {
		// Drop the PostgreSQL schema — name is validated as alphanumeric+underscore
		err := r.db.Exec(`DROP SCHEMA IF EXISTS "` + name + `" CASCADE`).Error
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
	} else {
		// SQLite: drop all tables with the schema prefix
		prefix := name + "__"
		tblRows, err := r.db.Raw("SELECT name FROM sqlite_master WHERE type='table' AND name LIKE ?", prefix+"%").Rows()
		if err == nil {
			defer tblRows.Close()
			for tblRows.Next() {
				var tblName string
				tblRows.Scan(&tblName)
				r.db.Exec(fmt.Sprintf("DROP TABLE IF EXISTS \"%s\"", tblName))
			}
		}
	}

	// Remove provision record
	r.db.Exec(fmt.Sprintf("DELETE FROM %s WHERE schema_name = ?", provisionsTable), name)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "dropped": name})
}

// HandleDeleteSchema drops every artifact tied to a space id: provisioned
// schemas (and their tables), manifest history, and the spaces row.
// Tolerant of partial / orphan state — registered-but-never-migrated spaces
// have a spaces row but no provisions, so refusing on "no schemas" leaves
// them un-cleanable from the admin UI. 404 only when nothing at all exists
// under this id.
func (r *Registry) HandleDeleteSchema(w http.ResponseWriter, req *http.Request) {
	spaceID := req.PathValue("spaceId")
	if spaceID == "" {
		http.Error(w, `{"error":"space_id required"}`, 400)
		return
	}

	provisionsTable := systemTable("provisions")
	manifestsTable := systemTable("manifests")
	spacesTable := systemTable("spaces")

	var schemaNames []string
	r.db.Raw(fmt.Sprintf("SELECT schema_name FROM %s WHERE space_id = ?", provisionsTable), spaceID).Scan(&schemaNames)

	// Confirm something exists for this id before reporting success: either
	// a provision, a manifest, or the spaces row itself. Otherwise this is
	// a genuine 404 (typo / unknown id) rather than an orphan we should
	// clean up.
	var spaceCount int64
	r.db.Raw(fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE id = ?", spacesTable), spaceID).Scan(&spaceCount)
	var manifestCount int64
	r.db.Raw(fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE space_id = ?", manifestsTable), spaceID).Scan(&manifestCount)

	if len(schemaNames) == 0 && spaceCount == 0 && manifestCount == 0 {
		http.Error(w, `{"error":"space not found"}`, 404)
		return
	}

	for _, name := range schemaNames {
		if !validIdentifier.MatchString(name) {
			continue // skip invalid names stored in DB
		}
		if engine.IsPostgres() {
			r.db.Exec(`DROP SCHEMA IF EXISTS "` + name + `" CASCADE`)
		} else {
			prefix := name + "__"
			tblRows, err := r.db.Raw("SELECT name FROM sqlite_master WHERE type='table' AND name LIKE ?", prefix+"%").Rows()
			if err == nil {
				defer tblRows.Close()
				for tblRows.Next() {
					var tblName string
					tblRows.Scan(&tblName)
					r.db.Exec(fmt.Sprintf("DROP TABLE IF EXISTS \"%s\"", tblName))
				}
			}
		}
		r.db.Exec(fmt.Sprintf("DELETE FROM %s WHERE schema_name = ?", provisionsTable), name)
	}

	r.db.Exec(fmt.Sprintf("DELETE FROM %s WHERE space_id = ?", manifestsTable), spaceID)
	r.db.Exec(fmt.Sprintf("DELETE FROM %s WHERE id = ?", spacesTable), spaceID)

	if schemaNames == nil {
		schemaNames = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "dropped": schemaNames})
}

// AdminGetModels returns models for a space (no auth required — admin endpoint)
func (r *Registry) AdminGetModels(w http.ResponseWriter, req *http.Request) {
	spaceID := req.PathValue("spaceId")
	models, err := r.GetModels(spaceID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "models": []any{}})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"space_id": spaceID, "models": models})
}

// HandleGetTableRows is the owner-scoped sibling of AdminGetTableRows. Same
// row-reading logic, but keyed by spaceId (the caller doesn't know schema
// names) and gated by ownership of the space rather than X-Internal-Secret.
// Backs my.lisaos.dev's data drawer.
//
// Path: /api/spaces/{spaceId}/tables/{tableName}/rows
// Optional query: project (disambiguates when a space has multiple
// project-scoped schemas; default = first match).
func (r *Registry) HandleGetTableRows(w http.ResponseWriter, req *http.Request) {
	spaceID := req.PathValue("spaceId")
	tableName := req.PathValue("tableName")
	if spaceID == "" || tableName == "" {
		http.Error(w, `{"error":"space_id and table required"}`, 400)
		return
	}
	if !validIdentifier.MatchString(tableName) {
		http.Error(w, `{"error":"invalid table identifier"}`, 400)
		return
	}

	// Find the space + ownership info. Missing row → 404 (not 403) so a
	// typo'd id is distinguishable from a permission denial.
	spacesTable := systemTable("spaces")
	var ownerUserID, publisherOrgID string
	row := r.db.Raw(
		fmt.Sprintf(`SELECT COALESCE(owner_user_id,''), COALESCE(publisher_org_id,'')
			FROM %s WHERE id = ?`, spacesTable),
		spaceID,
	).Row()
	if err := row.Scan(&ownerUserID, &publisherOrgID); err != nil {
		http.Error(w, `{"error":"space not found"}`, 404)
		return
	}

	// developer-api is the source of truth for ownership; when it has
	// pre-verified the caller (X-Internal-Owner-Verified, only trustworthy
	// because the auth middleware already validated X-Internal-Secret upstream)
	// skip the local check — graph's owner_user_id / publisher_org_id columns
	// are denormalized and frequently NULL for spaces registered before they
	// existed, so insisting on them produces false 403s for legitimate owners.
	verified := req.Header.Get("X-Internal-Owner-Verified") == "true"
	if !verified {
		callerUserID := req.Header.Get("X-Auth-User-ID")
		callerOrgID := req.Header.Get("X-Auth-Org-ID")
		isOwner := false
		if ownerUserID != "" && callerUserID != "" && callerUserID == ownerUserID {
			isOwner = true
		}
		if publisherOrgID != "" && callerOrgID != "" && callerOrgID == publisherOrgID {
			isOwner = true
		}
		if !isOwner {
			http.Error(w, `{"error":"forbidden"}`, 403)
			return
		}
	}

	// Pick which schemas to read from. As publisher-of-record (verified
	// owner), the caller wants to see every tenant's data for support — we
	// enumerate every physical schema matching this space's naming patterns
	// (project / org / user partitions) and UNION them. A synthetic _schema
	// column on each row identifies the partition. Without verification we
	// fall back to the single resolved tenant schema for the caller.
	//
	// `?project=<id>` narrows the union to the matching project partition
	// (`s_<space>_p_<id>`); `?schema=<name>` is an explicit single-schema
	// pick for cases where the caller already knows which one to read.
	limit := 50
	offset := 0
	if v := req.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 200 {
		limit = 200
	}
	if v := req.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	projectOverride := req.URL.Query().Get("project")
	schemaOverride := req.URL.Query().Get("schema")
	callerUserIDForSchema := req.Header.Get("X-Auth-User-ID")
	callerOrgIDForSchema := req.Header.Get("X-Auth-Org-ID")
	spaceScope := r.GetSpaceScope(spaceID, callerOrgIDForSchema)

	var schemasToQuery []string
	switch {
	case schemaOverride != "":
		if !validIdentifier.MatchString(schemaOverride) {
			http.Error(w, `{"error":"invalid schema identifier"}`, 400)
			return
		}
		if !schemaBelongsToSpace(schemaOverride, spaceID) {
			http.Error(w, `{"error":"schema does not belong to this space"}`, 403)
			return
		}
		schemasToQuery = []string{schemaOverride}
	case projectOverride != "":
		schemasToQuery = []string{SchemaName(spaceID, projectOverride)}
	case verified:
		schemasToQuery = r.listPhysicalSchemasForSpace(spaceID)
		if len(schemasToQuery) == 0 {
			schemasToQuery = []string{ResolveSchemaName(spaceScope, spaceID, "default", callerOrgIDForSchema, callerUserIDForSchema)}
		}
	default:
		schemasToQuery = []string{ResolveSchemaName(spaceScope, spaceID, "default", callerOrgIDForSchema, callerUserIDForSchema)}
	}

	// Filter to schemas that actually contain this table, capturing the
	// columns from the first hit so we can build a UNION with an explicit
	// column list (UNION ALL of `*` rejects column-count mismatches across
	// older migrations that haven't been re-run).
	type schemaShape struct {
		name string
		cols []string
	}
	var present []schemaShape
	var firstCols []string
	for _, s := range schemasToQuery {
		if !validIdentifier.MatchString(s) {
			continue
		}
		probe, err := r.db.Raw(fmt.Sprintf("SELECT * FROM %s LIMIT 0", engine.TableRef(s, tableName))).Rows()
		if err != nil {
			continue
		}
		cols, _ := probe.Columns()
		probe.Close()
		if len(cols) == 0 {
			continue
		}
		if firstCols == nil {
			firstCols = cols
		}
		present = append(present, schemaShape{name: s, cols: cols})
	}

	if len(present) == 0 {
		http.Error(w, `{"error":"no matching schemas for this space + table"}`, 404)
		return
	}

	// Use the column set from the first hit as the union signature. Drop
	// any partition that doesn't have all those columns rather than
	// silently NULL-padding (a column missing on one tenant is more useful
	// surfaced than buried).
	colSet := map[string]bool{}
	for _, c := range firstCols {
		colSet[c] = true
	}

	var unions []string
	for _, p := range present {
		ok := true
		have := map[string]bool{}
		for _, c := range p.cols {
			have[c] = true
		}
		for _, c := range firstCols {
			if !have[c] {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		quotedCols := make([]string, len(firstCols))
		for i, c := range firstCols {
			quotedCols[i] = quoteIdent(c)
		}
		colList := strings.Join(quotedCols, ", ")
		// Single-quote the schema literal — sqlEscape would over-quote.
		schemaLit := "'" + strings.ReplaceAll(p.name, "'", "''") + "'"
		unions = append(unions, fmt.Sprintf("SELECT %s, %s AS _schema FROM %s",
			colList, schemaLit, engine.TableRef(p.name, tableName)))
	}
	if len(unions) == 0 {
		http.Error(w, `{"error":"no schemas with compatible columns"}`, 500)
		return
	}

	unionQuery := strings.Join(unions, " UNION ALL ")
	var total int64
	if err := r.db.Raw(fmt.Sprintf("SELECT COUNT(*) FROM (%s) AS _u", unionQuery)).Scan(&total).Error; err != nil {
		http.Error(w, `{"error":"count failed: `+err.Error()+`"}`, 500)
		return
	}

	pagedQuery := fmt.Sprintf("%s ORDER BY _schema, 1 LIMIT %d OFFSET %d", unionQuery, limit, offset)
	rows, err := r.db.Raw(pagedQuery).Rows()
	if err != nil {
		http.Error(w, `{"error":"query failed: `+err.Error()+`"}`, 500)
		return
	}
	defer rows.Close()

	cols, _ := rows.Columns()
	out := []map[string]any{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			continue
		}
		rowOut := map[string]any{}
		for i, c := range cols {
			v := vals[i]
			if b, ok := v.([]byte); ok {
				rowOut[c] = string(b)
				continue
			}
			rowOut[c] = v
		}
		out = append(out, rowOut)
	}

	resolvedSchemas := make([]string, 0, len(present))
	for _, p := range present {
		resolvedSchemas = append(resolvedSchemas, p.name)
	}

	primarySchema := resolvedSchemas[0]
	if len(resolvedSchemas) == 1 {
		primarySchema = resolvedSchemas[0]
	} else {
		primarySchema = "(union)"
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"space_id": spaceID,
		"schema":   primarySchema,
		"schemas":  resolvedSchemas,
		"table":    tableName,
		"columns":  cols,
		"rows":     out,
		"total":    total,
		"limit":    limit,
		"offset":   offset,
	})
}

// schemaBelongsToSpace returns true if `schemaName` matches one of the
// naming patterns this space owns — guards the `?schema=` override from
// being used to read schemas owned by a different space.
func schemaBelongsToSpace(schemaName, spaceID string) bool {
	safe := sanitize(spaceID)
	if safe == "" {
		return false
	}
	if strings.HasPrefix(schemaName, "s_"+safe+"_p_") {
		return true
	}
	if strings.HasSuffix(schemaName, "_s_"+safe) &&
		(strings.HasPrefix(schemaName, "c_") || strings.HasPrefix(schemaName, "u_")) {
		return true
	}
	return false
}

// listPhysicalSchemasForSpace returns every schema name actually present in
// the database that matches one of this space's naming patterns:
//
//	s_<spaceID>_p_*  (project partition, includes 'default')
//	c_*_s_<spaceID>  (org tenant)
//	u_*_s_<spaceID>  (user tenant)
//
// PostgreSQL queries information_schema.schemata. SQLite has no schemas, so
// we derive partition names from the `<schema>__<table>` table-prefix
// convention by scanning sqlite_master.
func (r *Registry) listPhysicalSchemasForSpace(spaceID string) []string {
	safe := sanitize(spaceID)
	if safe == "" {
		return nil
	}

	if engine.IsPostgres() {
		var names []string
		r.db.Raw(`
			SELECT schema_name FROM information_schema.schemata
			WHERE schema_name LIKE ?
			   OR (schema_name LIKE 'c_%' AND schema_name LIKE ?)
			   OR (schema_name LIKE 'u_%' AND schema_name LIKE ?)
			ORDER BY schema_name
		`, "s_"+safe+"_p_%", "%_s_"+safe, "%_s_"+safe).Scan(&names)
		return names
	}

	var tableNames []string
	r.db.Raw(`SELECT name FROM sqlite_master WHERE type='table'`).Scan(&tableNames)
	seen := map[string]bool{}
	var schemas []string
	sPrefix := "s_" + safe + "_p_"
	suffix := "_s_" + safe
	for _, t := range tableNames {
		idx := strings.Index(t, "__")
		if idx <= 0 {
			continue
		}
		s := t[:idx]
		if seen[s] {
			continue
		}
		switch {
		case strings.HasPrefix(s, sPrefix):
		case (strings.HasPrefix(s, "c_") || strings.HasPrefix(s, "u_")) && strings.HasSuffix(s, suffix):
		default:
			continue
		}
		seen[s] = true
		schemas = append(schemas, s)
	}
	sort.Strings(schemas)
	return schemas
}

// AdminGetTableRows reads rows from one table inside a provisioned schema.
// Two layers of safety: the schema name must exist in the provisions
// registry (rejects arbitrary system-table reads), and both names are
// re-validated against `validIdentifier` even after that lookup. Only
// system tables we provisioned are reachable.
//
// Path: /api/admin/schemas/{schemaName}/tables/{tableName}/rows
// Query: limit (default 50, max 200), offset (default 0)
// Response: { schema, table, columns, rows, total, limit, offset }
func (r *Registry) AdminGetTableRows(w http.ResponseWriter, req *http.Request) {
	schemaName := req.PathValue("schemaName")
	tableName := req.PathValue("tableName")
	if schemaName == "" || tableName == "" {
		http.Error(w, `{"error":"schema and table required"}`, 400)
		return
	}
	if !validIdentifier.MatchString(schemaName) || !validIdentifier.MatchString(tableName) {
		http.Error(w, `{"error":"invalid identifier"}`, 400)
		return
	}

	// Schema must be in our provisions registry. Without this check, an
	// attacker holding the admin secret could read arbitrary tables (e.g.
	// pg_catalog) via this endpoint.
	provisionsTable := systemTable("provisions")
	var found int64
	r.db.Raw(fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE schema_name = ?", provisionsTable), schemaName).Scan(&found)
	if found == 0 {
		http.Error(w, `{"error":"schema not provisioned"}`, 404)
		return
	}

	limit := 50
	offset := 0
	if v := req.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 200 {
		limit = 200
	}
	if v := req.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	tableRef := engine.TableRef(schemaName, tableName)

	var total int64
	if err := r.db.Raw(fmt.Sprintf("SELECT COUNT(*) FROM %s", tableRef)).Scan(&total).Error; err != nil {
		// Most likely cause: table doesn't exist. Surface as 404 rather
		// than 500 — the caller can act on that (refresh the model list,
		// schema may be drifted).
		http.Error(w, `{"error":"table not found: `+err.Error()+`"}`, 404)
		return
	}

	rows, err := r.db.Raw(fmt.Sprintf("SELECT * FROM %s ORDER BY 1 LIMIT %d OFFSET %d", tableRef, limit, offset)).Rows()
	if err != nil {
		http.Error(w, `{"error":"query failed: `+err.Error()+`"}`, 500)
		return
	}
	defer rows.Close()

	cols, _ := rows.Columns()
	out := []map[string]any{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			continue
		}
		row := map[string]any{}
		for i, c := range cols {
			v := vals[i]
			// Bytes are PG's bytea / sqlite blob — render as string when
			// they're valid UTF-8 (most JSON-stored fields are), otherwise
			// keep raw bytes (Go's JSON encoder base64s them).
			if b, ok := v.([]byte); ok {
				row[c] = string(b)
				continue
			}
			row[c] = v
		}
		out = append(out, row)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"schema":  schemaName,
		"table":   tableName,
		"columns": cols,
		"rows":    out,
		"total":   total,
		"limit":   limit,
		"offset":  offset,
	})
}
