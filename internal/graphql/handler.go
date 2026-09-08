package graphql

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"construct-graph/internal/engine"
	"construct-graph/internal/schema"
)

// resolveOrgID decides which orgID to use for schema resolution.
// For org-scoped spaces, it trusts only the authenticated X-Auth-Org-ID
// (set by the auth middleware from accounts /api/me/scope). Personal
// callers (no org) on an org-scoped space fall through to a per-user
// bucket — handled by ResolveSchemaName via the userID parameter — so
// they never share a partition with other users. Non-org scopes return
// an empty orgID. Returns (orgID, httpStatus, errMsg) — status 0 means
// success.
func resolveOrgID(spaceScope, authScope, authOrgID string) (string, int, string) {
	if spaceScope != "org" {
		return "", 0, ""
	}
	// Personal scope on an org-scoped space → no orgID; ResolveSchemaName
	// will isolate them in their own user bucket.
	if authScope != "org" || authOrgID == "" {
		return "", 0, ""
	}
	return authOrgID, 0, ""
}

// Handler serves GraphQL requests
type Handler struct {
	registry    *schema.Registry
	engine      *engine.Engine
	accountsURL string
	eventHub    *EventHub
}

func NewHandler(registry *schema.Registry, eng *engine.Engine) *Handler {
	return &Handler{registry: registry, engine: eng}
}

// SetAccountsURL sets the accounts service URL for membership checks
func (h *Handler) SetAccountsURL(url string) {
	h.accountsURL = url
}

// GraphQL request
type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

// requestContext holds per-request auth and scope info
type requestContext struct {
	spaceID   string
	projectID string
	userID    string
	orgID     string   // auth-attested org (accounts /me/scope). "" for personal scope.
	roles     []string // auth-attested roles within orgID (source: X-Auth-Roles header).
	schema    string
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Extract space/project from headers (client-supplied context).
	spaceID := r.Header.Get("X-Space-ID")
	projectID := r.Header.Get("X-Project-ID")
	if spaceID == "" {
		writeError(w, "X-Space-ID header required", 400)
		return
	}
	if projectID == "" {
		projectID = "default"
	}

	// Authenticated identity comes from the auth middleware — never from
	// client-supplied X-Company-ID, which would let any caller read another
	// org's data by picking its ID.
	userID := r.Header.Get("X-Auth-User-ID")
	authScope := r.Header.Get("X-Auth-Scope")
	authOrgID := r.Header.Get("X-Auth-Org-ID")

	// Pass the caller's authenticated org (if any) so a space declaring
	// both "app" and "org" scopes resolves to "org" only for org callers.
	// Personal callers land in "app" and isolate per user — without this,
	// the registry returned "" and we fell through to the shared project
	// schema, leaking everyone's data into one partition.
	spaceScope := h.registry.GetSpaceScope(spaceID, authOrgID)
	orgID, status, errMsg := resolveOrgID(spaceScope, authScope, authOrgID)
	if status != 0 {
		writeError(w, errMsg, status)
		return
	}

	// Install gate — a space with a publisher_org_id is an "owned" space under
	// the new model (Phase 1+). Org callers must have installed it first unless
	// they are the publisher themselves. Spaces without a publisher_org_id are
	// legacy/standalone and skip this check for backward compatibility.
	if authOrgID != "" {
		if publisher := h.registry.GetSpacePublisherOrg(spaceID); publisher != "" && publisher != authOrgID {
			if !h.registry.IsInstalled(spaceID, authOrgID) {
				writeError(w, "space not installed for this org — POST /api/spaces/"+spaceID+"/install first", 403)
				return
			}
		}
	}

	schemaName := schema.ResolveSchemaName(spaceScope, spaceID, projectID, orgID, userID)

	// Try to get existing schema, fall back to computed name. Per-tenant
	// scopes (org / app) always recompute since their schema embeds the
	// tenant id; only "project" / unknown share a stable schema name.
	if spaceScope != "org" && spaceScope != "app" {
		if existingSchema, err := h.registry.GetSchemaName(spaceID, projectID); err == nil {
			schemaName = existingSchema
		}
	} else {
		// Per-tenant partition — provision schema + tables on first hit.
		// Push only creates tables under the publisher's partition; every
		// other tenant lazily materializes here so they don't see "relation
		// does not exist" on first query.
		if err := h.registry.EnsureTenantSchema(spaceID, schemaName); err != nil {
			writeError(w, fmt.Sprintf("provision tenant schema: %v", err), 500)
			return
		}
	}

	// Parse request
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, "bad request", 400)
		return
	}

	var req gqlRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, "invalid json", 400)
		return
	}

	var roles []string
	if raw := r.Header.Get("X-Auth-Roles"); raw != "" {
		for _, rname := range strings.Split(raw, ",") {
			if s := strings.TrimSpace(rname); s != "" {
				roles = append(roles, s)
			}
		}
	}

	rctx := &requestContext{
		spaceID:   spaceID,
		projectID: projectID,
		userID:    userID,
		orgID:     authOrgID,
		roles:     roles,
		schema:    schemaName,
	}

	// Resolve with access control
	result, err := h.resolve(r.Context(), rctx, req)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data":   nil,
			"errors": []map[string]any{{"message": err.Error()}},
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"data": result})
}

// publisherAdminRoles lists org roles that grant publisher_admin capability.
// Kept centralized so a future change to naming (e.g. "maintainer") updates
// every callsite. Roles are matched case-insensitively.
var publisherAdminRoles = []string{"owner", "admin", "developer"}

// hasPublisherAdminRole reports whether any of the caller's roles qualifies
// for publisher_admin access. Case-insensitive.
func hasPublisherAdminRole(roles []string) bool {
	for _, r := range roles {
		low := strings.ToLower(strings.TrimSpace(r))
		for _, allowed := range publisherAdminRoles {
			if low == allowed {
				return true
			}
		}
	}
	return false
}

// resolveSchemaForModel returns the physical schema where a model's tables
// live. For locally-defined models that's the caller's own tenant schema;
// for imported models it's the source space's tenant schema (same project_id,
// different space). Relations inherit the parent model's schema.
func (h *Handler) resolveSchemaForModel(rctx *requestContext, modelName string) string {
	resolved := h.registry.ResolveModel(rctx.spaceID, modelName)
	if resolved == nil || !resolved.Imported {
		return rctx.schema
	}
	// Imported model — compute the source space's schema in the same tenant
	// context. Prefer the provisioned schema if one exists (survives renames);
	// fall back to the deterministic name.
	if name, err := h.registry.GetSchemaName(resolved.SourceSpaceID, rctx.projectID); err == nil && name != "" {
		return name
	}
	scope := h.registry.GetSpaceScope(resolved.SourceSpaceID, rctx.orgID)
	return schema.ResolveSchemaName(scope, resolved.SourceSpaceID, rctx.projectID, rctx.orgID, rctx.userID)
}

func (h *Handler) resolveModelDef(spaceID, modelName string) (schema.ModelDef, bool) {
	resolved := h.registry.ResolveModel(spaceID, modelName)
	if resolved == nil {
		return schema.ModelDef{}, false
	}
	return resolved.Model, true
}

func (h *Handler) normalizeJSONInput(rctx *requestContext, modelName string, input map[string]any) error {
	model, ok := h.resolveModelDef(rctx.spaceID, modelName)
	if !ok {
		return nil
	}
	return normalizeJSONInputForModel(model, input)
}

func (h *Handler) normalizeJSONRecord(rctx *requestContext, modelName string, row map[string]any) {
	model, ok := h.resolveModelDef(rctx.spaceID, modelName)
	if !ok {
		return
	}
	normalizeJSONRecordForModel(model, row)
}

func (h *Handler) normalizeJSONRecords(rctx *requestContext, modelName string, rows []map[string]any) {
	model, ok := h.resolveModelDef(rctx.spaceID, modelName)
	if !ok {
		return
	}
	normalizeJSONRecordsForModel(model, rows)
}

// checkAccess validates whether the operation is allowed based on model access rules.
// Rules are read from the importing space when the model is imported (so the
// importer can override with e.g. `publisher_admin`); if the importer didn't
// redeclare rules, we fall back to the source space's rules.
func (h *Handler) checkAccess(rctx *requestContext, modelName, operation string) error {
	rules := h.registry.GetModelAccess(rctx.spaceID, modelName)
	if rules == nil {
		// Fall back to source space's rules if this is an imported model.
		if resolved := h.registry.ResolveModel(rctx.spaceID, modelName); resolved != nil && resolved.Imported {
			rules = h.registry.GetModelAccess(resolved.SourceSpaceID, modelName)
		}
	}
	if rules == nil {
		// No access rules defined — default to authenticated (backward compat)
		if rctx.userID == "" {
			return fmt.Errorf("authentication required")
		}
		return nil
	}

	var level string
	switch operation {
	case "read":
		level = rules.Read
	case "create":
		level = rules.Create
	case "update":
		level = rules.Update
	case "delete":
		level = rules.Delete
	default:
		level = "authenticated"
	}

	if level == "" {
		level = "authenticated" // default
	}

	switch level {
	case "public":
		return nil // anyone can access
	case "none":
		return fmt.Errorf("operation %s not allowed on %s", operation, modelName)
	case "authenticated":
		if rctx.userID == "" {
			return fmt.Errorf("authentication required")
		}
	case "owner":
		if rctx.userID == "" {
			return fmt.Errorf("authentication required")
		}
		// Owner filtering is applied in the query layer (created_by = userID)
	case "member":
		if rctx.userID == "" {
			return fmt.Errorf("authentication required")
		}
		// Org-scoped spaces with a member-only access rule require an actual
		// org session — personal callers can still read their own per-user
		// bucket via "owner" / "authenticated" rules but not a member rule.
		if rctx.orgID == "" {
			scope := h.registry.GetSpaceScope(rctx.spaceID, rctx.orgID)
			if scope == "org" {
				return fmt.Errorf("org context required for member access")
			}
		}
	case "admin":
		if rctx.userID == "" {
			return fmt.Errorf("authentication required")
		}
		// TODO: Check admin role via accounts API
		// For now, admin is not enforced beyond authentication
	case "publisher_admin":
		// Caller must be in the publishing org (the org that owns the space's
		// bundle) AND hold an admin-class role in that org. Intended for
		// publisher-operated admin spaces — e.g. a kanban-admin space that
		// reads aggregate install metadata. Row-level tenant filters are
		// skipped for this level (applyOwnerFilter).
		//
		// Roles come from accounts /me/scope and are attested via the
		// X-Auth-Roles header set by graph's auth middleware (see auth.go).
		// "owner", "admin", and "developer" all qualify — these are the
		// role names source.lisaos.dev assigns for publish + manage
		// capabilities.
		if rctx.userID == "" {
			return fmt.Errorf("authentication required")
		}
		publisherOrg := h.registry.GetSpacePublisherOrg(rctx.spaceID)
		if publisherOrg == "" {
			return fmt.Errorf("publisher_admin access requires a publisher_org_id on the space")
		}
		if rctx.orgID == "" {
			return fmt.Errorf("publisher_admin access requires an authenticated org context")
		}
		if rctx.orgID != publisherOrg {
			return fmt.Errorf("publisher_admin access: caller org does not match publisher")
		}
		if !hasPublisherAdminRole(rctx.roles) {
			return fmt.Errorf("publisher_admin access: caller lacks admin role in publisher org")
		}
	}

	return nil
}

func (h *Handler) resolve(ctx context.Context, rctx *requestContext, req gqlRequest) (map[string]any, error) {
	query := strings.TrimSpace(req.Query)
	vars := req.Variables

	// Detect mutation vs query
	if strings.HasPrefix(query, "mutation") {
		return h.resolveMutation(ctx, rctx, query, vars)
	}
	return h.resolveQuery(ctx, rctx, query, vars)
}

func (h *Handler) applyOwnerFilter(rctx *requestContext, modelName string, vars map[string]any) map[string]any {
	if vars == nil {
		vars = make(map[string]any)
	}

	// Check if this model uses owner-based access for reads.
	// For imported models, fall back to the source space's rules when the
	// importing space hasn't redeclared them.
	rules := h.registry.GetModelAccess(rctx.spaceID, modelName)
	if rules == nil {
		if resolved := h.registry.ResolveModel(rctx.spaceID, modelName); resolved != nil && resolved.Imported {
			rules = h.registry.GetModelAccess(resolved.SourceSpaceID, modelName)
		}
	}

	// publisher_admin reads across tenants — suppress tenant filter entirely.
	if rules != nil && rules.Read == "publisher_admin" {
		return vars
	}

	useOwnerFilter := false
	if rules != nil && rules.Read == "owner" {
		useOwnerFilter = true
	} else if rules == nil && rctx.userID != "" {
		// Backward compat: no rules defined = filter by created_by
		useOwnerFilter = true
	}

	if useOwnerFilter && rctx.userID != "" {
		if where, ok := vars["where"].(map[string]any); ok {
			where["created_by"] = rctx.userID
		} else {
			vars["where"] = map[string]any{"created_by": rctx.userID}
		}
	}

	return vars
}

// buildRelationSpecs returns JoinSpecs for requested nested fields that match model relations
func (h *Handler) buildRelationSpecs(spaceID, modelName string, requestedFields []string) (joins []engine.RelationSpec, hasMany []engine.RelationSpec) {
	relFields := h.registry.GetRelationFields(spaceID, modelName)
	requested := make(map[string]bool)
	for _, f := range requestedFields {
		requested[f] = true
	}

	for _, f := range relFields {
		if !requested[f.Name] {
			continue
		}
		spec := engine.RelationSpec{
			FieldName: f.Name,
			Target:    sanitizeGQL(f.Target),
			Type:      f.Relation,
		}
		switch f.Relation {
		case "belongsTo":
			spec.FKColumn = sanitizeGQL(f.Name) + "_id"
			joins = append(joins, spec)
		case "hasMany":
			spec.FKColumn = sanitizeGQL(modelName) + "_id"
			hasMany = append(hasMany, spec)
		}
	}
	return
}

func sanitizeGQL(s string) string {
	out := make([]byte, 0, len(s))
	for _, b := range []byte(s) {
		if (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_' {
			out = append(out, b)
		} else if b >= 'A' && b <= 'Z' {
			out = append(out, b+32)
		} else if b == '-' {
			out = append(out, '_')
		}
	}
	return string(out)
}

func (h *Handler) resolveQuery(ctx context.Context, rctx *requestContext, query string, vars map[string]any) (map[string]any, error) {
	result := make(map[string]any)
	op, args := parseOperation(query)

	// Built-in platform queries (prefixed with `_`) — surface publisher-level
	// metadata that isn't part of any space's model schema. Today only one
	// query is exposed: `_installs`, which lists orgs that installed a space
	// in the caller's product.
	if strings.HasPrefix(op, "_") {
		return h.resolveBuiltin(ctx, rctx, op, args, vars)
	}

	if strings.HasSuffix(op, "Count") || strings.HasSuffix(op, "count") {
		table := strings.TrimSuffix(strings.TrimSuffix(op, "Count"), "count")
		table = strings.TrimSuffix(table, "s")

		if err := h.checkAccess(rctx, table, "read"); err != nil {
			return nil, err
		}
		vars = h.applyOwnerFilter(rctx, table, vars)

		where := extractWhere(args, vars)
		count, err := h.engine.Count(ctx, h.resolveSchemaForModel(rctx, table), table, where)
		if err != nil {
			return nil, err
		}
		result[op] = count
	} else if table, ok := strings.CutSuffix(op, "s"); ok {

		if err := h.checkAccess(rctx, table, "read"); err != nil {
			return nil, err
		}
		vars = h.applyOwnerFilter(rctx, table, vars)

		where := extractWhere(args, vars)
		orderBy := extractOrderBy(args, vars)
		limit := extractInt(args, vars, "limit", 100)
		offset := extractInt(args, vars, "offset", 0)

		// Check for nested relation fields
		nestedFields := parseNestedFields(query)
		joinSpecs, hasManySpecs := h.buildRelationSpecs(rctx.spaceID, table, nestedFields)

		var rows []map[string]any
		var err error

		modelSchema := h.resolveSchemaForModel(rctx, table)
		if len(joinSpecs) > 0 {
			rows, err = h.engine.FindManyWithJoins(ctx, modelSchema, table, where, orderBy, limit, offset, joinSpecs)
		} else {
			rows, err = h.engine.FindMany(ctx, modelSchema, table, where, orderBy, limit, offset)
		}
		if err != nil {
			return nil, err
		}
		h.normalizeJSONRecords(rctx, table, rows)

		// Batch-load hasMany relations
		if len(hasManySpecs) > 0 && len(rows) > 0 {
			parentIDs := make([]string, 0, len(rows))
			for _, row := range rows {
				if id, ok := row["id"]; ok {
					parentIDs = append(parentIDs, fmt.Sprint(id))
				}
			}

			for _, spec := range hasManySpecs {
				grouped, err := h.engine.BatchLoadRelated(ctx, modelSchema, spec.Target, spec.FKColumn, parentIDs)
				if err != nil {
					return nil, err
				}
				for _, row := range rows {
					id := fmt.Sprint(row["id"])
					if related, ok := grouped[id]; ok {
						row[spec.FieldName] = related
					} else {
						row[spec.FieldName] = []map[string]any{}
					}
				}
			}
		}

		result[op] = rows
	} else {
		if err := h.checkAccess(rctx, op, "read"); err != nil {
			return nil, err
		}

		id := extractString(args, vars, "id")
		if id == "" {
			return nil, fmt.Errorf("id required for single record query")
		}
		modelSchema := h.resolveSchemaForModel(rctx, op)
		row, err := h.engine.FindOne(ctx, modelSchema, op, id)
		if err != nil {
			return nil, err
		}
		if row == nil {
			result[op] = nil
			return result, nil
		}
		h.normalizeJSONRecord(rctx, op, row)

		// Resolve nested relations for single record
		nestedFields := parseNestedFields(query)
		joinSpecs, hasManySpecs := h.buildRelationSpecs(rctx.spaceID, op, nestedFields)

		// belongsTo: fetch related record by FK
		for _, spec := range joinSpecs {
			fkVal := fmt.Sprint(row[spec.FKColumn])
			if fkVal != "" && fkVal != "<nil>" {
				related, err := h.engine.FindOne(ctx, modelSchema, spec.Target, fkVal)
				if err == nil && related != nil {
					row[spec.FieldName] = related
				}
			}
		}

		// hasMany: batch load (single parent)
		for _, spec := range hasManySpecs {
			parentID := fmt.Sprint(row["id"])
			grouped, err := h.engine.BatchLoadRelated(ctx, modelSchema, spec.Target, spec.FKColumn, []string{parentID})
			if err == nil {
				if related, ok := grouped[parentID]; ok {
					row[spec.FieldName] = related
				} else {
					row[spec.FieldName] = []map[string]any{}
				}
			}
		}

		result[op] = row
	}

	return result, nil
}

func (h *Handler) resolveMutation(ctx context.Context, rctx *requestContext, query string, vars map[string]any) (map[string]any, error) {
	result := make(map[string]any)
	op, _ := parseOperation(query)

	if table, ok := strings.CutPrefix(op, "create"); ok {
		table = strings.ToLower(table[:1]) + table[1:]

		if err := h.checkAccess(rctx, table, "create"); err != nil {
			return nil, err
		}

		input, _ := vars["input"].(map[string]any)
		if input == nil {
			return nil, fmt.Errorf("input variable required")
		}
		if rctx.userID != "" {
			input["created_by"] = rctx.userID
		}
		if err := h.normalizeJSONInput(rctx, table, input); err != nil {
			return nil, err
		}
		row, err := h.engine.Create(ctx, h.resolveSchemaForModel(rctx, table), table, input)
		if err != nil {
			return nil, err
		}
		h.normalizeJSONRecord(rctx, table, row)
		result[op] = row
	} else if table, ok := strings.CutPrefix(op, "update"); ok {
		table = strings.ToLower(table[:1]) + table[1:]

		if err := h.checkAccess(rctx, table, "update"); err != nil {
			return nil, err
		}

		id, _ := vars["id"].(string)
		input, _ := vars["input"].(map[string]any)
		if id == "" || input == nil {
			return nil, fmt.Errorf("id and input variables required")
		}
		if err := h.normalizeJSONInput(rctx, table, input); err != nil {
			return nil, err
		}

		// For owner access: verify the record belongs to this user
		rules := h.registry.GetModelAccess(rctx.spaceID, table)
		modelSchema := h.resolveSchemaForModel(rctx, table)
		if rules != nil && rules.Update == "owner" && rctx.userID != "" {
			existing, err := h.engine.FindOne(ctx, modelSchema, table, id)
			if err != nil {
				return nil, err
			}
			if existing != nil {
				if createdBy, ok := existing["created_by"].(string); ok && createdBy != rctx.userID {
					return nil, fmt.Errorf("forbidden: you can only update your own records")
				}
			}
		}

		row, err := h.engine.Update(ctx, modelSchema, table, id, input)
		if err != nil {
			return nil, err
		}
		h.normalizeJSONRecord(rctx, table, row)
		result[op] = row
	} else if table, ok := strings.CutPrefix(op, "delete"); ok {
		table = strings.ToLower(table[:1]) + table[1:]

		if err := h.checkAccess(rctx, table, "delete"); err != nil {
			return nil, err
		}

		id, _ := vars["id"].(string)
		if id == "" {
			return nil, fmt.Errorf("id variable required")
		}

		// For owner access: verify the record belongs to this user
		rules := h.registry.GetModelAccess(rctx.spaceID, table)
		modelSchema := h.resolveSchemaForModel(rctx, table)
		if rules != nil && rules.Delete == "owner" && rctx.userID != "" {
			existing, err := h.engine.FindOne(ctx, modelSchema, table, id)
			if err != nil {
				return nil, err
			}
			if existing != nil {
				if createdBy, ok := existing["created_by"].(string); ok && createdBy != rctx.userID {
					return nil, fmt.Errorf("forbidden: you can only delete your own records")
				}
			}
		}

		err := h.engine.Delete(ctx, modelSchema, table, id)
		if err != nil {
			return nil, err
		}
		result[op] = true
	}

	return result, nil
}

// Simplified GraphQL operation parser
func parseOperation(query string) (string, string) {
	// Strip query/mutation keyword and braces
	q := strings.TrimSpace(query)
	q = strings.TrimPrefix(q, "query")
	q = strings.TrimPrefix(q, "mutation")
	q = strings.TrimSpace(q)

	// Find first operation name
	_, inner, ok := strings.Cut(q, "{")
	if !ok {
		return "", ""
	}

	// Find operation name
	inner = strings.TrimSpace(inner)
	parenIdx := strings.Index(inner, "(")
	braceIdx := strings.Index(inner, "{")
	spaceIdx := strings.Index(inner, " ")

	endIdx := len(inner)
	for _, idx := range []int{parenIdx, braceIdx, spaceIdx} {
		if idx > 0 && idx < endIdx {
			endIdx = idx
		}
	}

	op := strings.TrimSpace(inner[:endIdx])

	// Extract args between parentheses
	args := ""
	if parenIdx > 0 {
		closeIdx := findMatchingParen(inner, parenIdx)
		if closeIdx > parenIdx {
			args = inner[parenIdx+1 : closeIdx]
		}
	}

	return op, args
}

func findMatchingParen(s string, openIdx int) int {
	depth := 0
	for i := openIdx; i < len(s); i++ {
		switch s[i] {
		case '(', '{':
			depth++
		case ')', '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// `args` is reserved for a future GraphQL parser that pulls args out of the
// query string; variables-only extraction is all we use today.
func extractWhere(_ string, vars map[string]any) map[string]any {
	if w, ok := vars["where"].(map[string]any); ok {
		return w
	}
	return nil
}

func extractOrderBy(_ string, vars map[string]any) map[string]string {
	if o, ok := vars["orderBy"].(map[string]any); ok {
		result := make(map[string]string)
		for k, v := range o {
			result[k] = fmt.Sprint(v)
		}
		return result
	}
	return nil
}

func extractInt(_ string, vars map[string]any, key string, defaultVal int) int {
	if v, ok := vars[key]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		}
	}
	return defaultVal
}

func extractString(_ string, vars map[string]any, key string) string {
	if v, ok := vars[key].(string); ok {
		return v
	}
	return ""
}

// parseNestedFields extracts nested field names from a GraphQL query.
// e.g. "{ cards { id title elements { id component } } }" returns ["elements"]
func parseNestedFields(query string) []string {
	var nested []string
	depth := 0
	inMainBody := false
	current := ""
	runes := []rune(query)

	for i := 0; i < len(runes); i++ {
		ch := runes[i]
		switch ch {
		case '{':
			depth++
			if depth == 2 {
				inMainBody = true
				current = ""
			} else if depth == 3 && inMainBody {
				name := strings.TrimSpace(current)
				if paren := strings.Index(name, "("); paren > 0 {
					name = name[:paren]
				}
				if name != "" {
					nested = append(nested, name)
				}
				current = ""
			}
		case '}':
			depth--
			current = ""
		case '\n', '\r':
			if inMainBody && depth == 2 {
				current = ""
			}
		default:
			if inMainBody && depth == 2 {
				current += string(ch)
			}
		}
	}
	return nested
}

func writeError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]any{{"message": msg}},
	})
}

// PlaygroundHandler returns a GraphiQL playground with editable headers
func PlaygroundHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<!DOCTYPE html>
<html><head>
<title>Construct Graph — GraphQL Playground</title>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/graphiql@3.8.3/graphiql.min.css" />
<style>
  body { margin: 0; overflow: hidden; }
  #header-bar {
    display: flex; align-items: center; gap: 8px;
    padding: 6px 12px; background: #1a1a2e; border-bottom: 1px solid #333;
    font-family: system-ui, sans-serif; font-size: 13px; color: #ccc;
  }
  #header-bar label { color: #888; font-size: 11px; text-transform: uppercase; letter-spacing: 0.5px; }
  #header-bar input {
    background: #0d0d1a; border: 1px solid #333; color: #e0e0e0;
    padding: 4px 8px; border-radius: 4px; font-size: 12px; font-family: monospace; width: 180px;
  }
  #header-bar .brand { font-weight: 600; color: #7c3aed; margin-right: 12px; }
  #header-bar .sep { width: 1px; height: 20px; background: #333; }
</style>
</head><body>
<div id="header-bar">
  <span class="brand">CONSTRUCT:GRAPH</span>
  <div class="sep"></div>
  <label>Space ID</label>
  <input id="h-space" value="test" onchange="updateHeaders()" />
  <label>Project ID</label>
  <input id="h-project" value="test" onchange="updateHeaders()" />
  <div class="sep"></div>
  <label>Auth Token</label>
  <input id="h-token" placeholder="Bearer token (optional in dev mode)" style="width:280px" onchange="updateHeaders()" />
</div>
<div id="graphiql" style="height:calc(100vh - 41px)"></div>
<script crossorigin src="https://cdn.jsdelivr.net/npm/react@18.3.1/umd/react.production.min.js"></script>
<script crossorigin src="https://cdn.jsdelivr.net/npm/react-dom@18.3.1/umd/react-dom.production.min.js"></script>
<script crossorigin src="https://cdn.jsdelivr.net/npm/graphiql@3.8.3/graphiql.min.js"></script>
<script>
function getHeaders() {
  const h = {
    'X-Space-ID': document.getElementById('h-space').value || 'test',
    'X-Project-ID': document.getElementById('h-project').value || 'test',
  };
  const token = document.getElementById('h-token').value.trim();
  if (token) h['Authorization'] = token.startsWith('Bearer ') ? token : 'Bearer ' + token;
  return h;
}

function renderGraphiQL() {
  const el = React.createElement(GraphiQL, {
    fetcher: GraphiQL.createFetcher({ url: '/graphql', headers: getHeaders() }),
    defaultQuery: '# Register a schema first, then query it:\n#\n#   POST /api/schemas/register\n#   { "space_id": "test", "project_id": "test", "version": "1.0.0",\n#     "manifest": { "version": 1, "models": [\n#       { "name": "task", "fields": [\n#         { "name": "title", "type": "string", "required": true },\n#         { "name": "done", "type": "boolean", "default": false }\n#       ]}\n#     ]}}\n#\n# Then query:\n#   { tasks { id title done created_at } }\n#\n# Or create:\n#   mutation { createTask { id title } }\n#   variables: { "input": { "title": "My first task" } }\n\n{ tasks { id title done created_at } }\n',
  });
  ReactDOM.render(el, document.getElementById('graphiql'));
}

function updateHeaders() { renderGraphiQL(); }
renderGraphiQL();
</script></body></html>`))
	})
}
