# GraphQL Relations (JOIN-based) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add relation resolution to the GraphQL layer using SQL JOINs for belongsTo and batch IN queries for hasMany, avoiding N+1.

**Architecture:** The manifest already defines relations on FieldDef (Relation, Target, OnDelete). We add a `GetModelFields` method to the registry so the resolver can discover relations at query time. The engine gets a new `FindManyWithRelations` method that builds LEFT JOIN queries for belongsTo fields and a batch loader for hasMany. The GraphQL handler parses requested nested fields from the query string and passes relation specs to the engine.

**Tech Stack:** Go, raw SQL (Postgres + SQLite compat), GORM (db.Raw only)

---

### Task 1: Registry — Expose model field definitions

The resolver needs to look up which fields are relations and what they target. Add `GetModelFields` to the registry.

**Files:**
- Modify: `internal/schema/registry.go` (add method after `GetModelAccess` ~line 157)

- [ ] **Step 1: Add GetModelFields method**

```go
// Add after GetModelAccess (line 157) in registry.go

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
```

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/flakerim/Construct/infra/graph && go build ./...`
Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add internal/schema/registry.go
git commit -m "feat: expose model field definitions from registry"
```

---

### Task 2: Engine — FindManyWithJoins for belongsTo

Add a method that builds LEFT JOIN queries for belongsTo relations. This handles the parent→child direction in a single SQL query.

**Files:**
- Modify: `internal/engine/engine.go` (add new method + helper)

- [ ] **Step 1: Define RelationSpec type and FindManyWithJoins method**

Add after the `FindMany` method (~line 120):

```go
// RelationSpec describes a relation to JOIN or batch-load
type RelationSpec struct {
	FieldName string // e.g. "card"
	Target    string // target table name e.g. "card"
	Type      string // "belongsTo" or "hasMany"
	FKColumn  string // foreign key column: "card_id" for belongsTo, "element_id" for hasMany
}

// FindManyWithJoins queries records with LEFT JOINs for belongsTo relations
func (e *Engine) FindManyWithJoins(ctx context.Context, schemaName, tableName string, where map[string]any, orderBy map[string]string, limit, offset int, joins []RelationSpec) ([]map[string]any, error) {
	if len(joins) == 0 {
		return e.FindMany(ctx, schemaName, tableName, where, orderBy, limit, offset)
	}

	table := TableRef(schemaName, tableName)
	alias := "t"

	// Build SELECT: t.*, row_to_json(j0.*) AS "card", ...
	selectParts := []string{alias + ".*"}
	joinClauses := []string{}

	for i, j := range joins {
		if j.Type != "belongsTo" {
			continue
		}
		joinAlias := fmt.Sprintf("j%d", i)
		joinTable := TableRef(schemaName, j.Target)

		if IsPostgres() {
			selectParts = append(selectParts, fmt.Sprintf("row_to_json(%s.*) AS %q", joinAlias, j.FieldName))
		} else {
			// SQLite: no row_to_json, select id only as fallback
			selectParts = append(selectParts, fmt.Sprintf("%s.id AS %q", joinAlias, j.FieldName+"_ref"))
		}

		joinClauses = append(joinClauses,
			fmt.Sprintf("LEFT JOIN %s %s ON %s.id = %s.%s", joinTable, joinAlias, joinAlias, alias, j.FKColumn))
	}

	query := fmt.Sprintf("SELECT %s FROM %s %s %s",
		strings.Join(selectParts, ", "),
		table, alias,
		strings.Join(joinClauses, " "))

	// WHERE
	var conditions []string
	var args []any
	for field, value := range where {
		if m, ok := value.(map[string]any); ok {
			for op, val := range m {
				switch op {
				case "$gt":
					conditions = append(conditions, fmt.Sprintf("%s.%s > ?", alias, field))
				case "$gte":
					conditions = append(conditions, fmt.Sprintf("%s.%s >= ?", alias, field))
				case "$lt":
					conditions = append(conditions, fmt.Sprintf("%s.%s < ?", alias, field))
				case "$lte":
					conditions = append(conditions, fmt.Sprintf("%s.%s <= ?", alias, field))
				case "$ne":
					conditions = append(conditions, fmt.Sprintf("%s.%s != ?", alias, field))
				case "$like":
					conditions = append(conditions, fmt.Sprintf("%s.%s LIKE ?", alias, field))
				case "$in":
					if arr, ok := val.([]any); ok {
						placeholders := make([]string, len(arr))
						for i, v := range arr {
							placeholders[i] = "?"
							args = append(args, v)
						}
						conditions = append(conditions, fmt.Sprintf("%s.%s IN (%s)", alias, field, strings.Join(placeholders, ",")))
						continue
					}
				case "$null":
					if val == true {
						conditions = append(conditions, fmt.Sprintf("%s.%s IS NULL", alias, field))
					} else {
						conditions = append(conditions, fmt.Sprintf("%s.%s IS NOT NULL", alias, field))
					}
					continue
				}
				args = append(args, val)
			}
		} else {
			conditions = append(conditions, fmt.Sprintf("%s.%s = ?", alias, field))
			args = append(args, value)
		}
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}

	// ORDER BY
	if len(orderBy) > 0 {
		var orders []string
		for field, dir := range orderBy {
			d := "ASC"
			if strings.ToLower(dir) == "desc" {
				d = "DESC"
			}
			orders = append(orders, fmt.Sprintf("%s.%s %s", alias, field, d))
		}
		query += " ORDER BY " + strings.Join(orders, ", ")
	} else {
		query += fmt.Sprintf(" ORDER BY %s.created_at DESC", alias)
	}

	// LIMIT + OFFSET
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	} else {
		query += " LIMIT 100"
	}
	if offset > 0 {
		query += fmt.Sprintf(" OFFSET %d", offset)
	}

	rows, err := e.db.Raw(query, args...).Rows()
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	return scanRows(rows)
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/flakerim/Construct/infra/graph && go build ./...`
Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add internal/engine/engine.go
git commit -m "feat: add FindManyWithJoins for belongsTo relations"
```

---

### Task 3: Engine — BatchLoadRelated for hasMany

Add a method that loads related records for a set of parent IDs using a single `WHERE fk IN (...)` query. This is the hasMany side — exactly 2 queries total (parent rows + all children).

**Files:**
- Modify: `internal/engine/engine.go`

- [ ] **Step 1: Add BatchLoadRelated method**

Add after `FindManyWithJoins`:

```go
// BatchLoadRelated loads related records for multiple parent IDs using IN query.
// Returns map[parentID] -> []related records
func (e *Engine) BatchLoadRelated(ctx context.Context, schemaName, targetTable, fkColumn string, parentIDs []string) (map[string][]map[string]any, error) {
	if len(parentIDs) == 0 {
		return nil, nil
	}

	table := TableRef(schemaName, targetTable)
	placeholders := make([]string, len(parentIDs))
	args := make([]any, len(parentIDs))
	for i, id := range parentIDs {
		placeholders[i] = "?"
		args[i] = id
	}

	query := fmt.Sprintf("SELECT * FROM %s WHERE %s IN (%s) ORDER BY created_at DESC",
		table, fkColumn, strings.Join(placeholders, ","))

	rows, err := e.db.Raw(query, args...).Rows()
	if err != nil {
		return nil, fmt.Errorf("batch load: %w", err)
	}
	defer rows.Close()

	allRows, err := scanRows(rows)
	if err != nil {
		return nil, err
	}

	// Group by foreign key
	grouped := make(map[string][]map[string]any)
	for _, row := range allRows {
		fkVal := fmt.Sprint(row[fkColumn])
		grouped[fkVal] = append(grouped[fkVal], row)
	}
	return grouped, nil
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/flakerim/Construct/infra/graph && go build ./...`
Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add internal/engine/engine.go
git commit -m "feat: add BatchLoadRelated for hasMany relations"
```

---

### Task 4: GraphQL handler — Parse nested fields from query

The resolver needs to know which nested fields (relations) the client requested. Add a parser that extracts nested field names from the GraphQL query string.

**Files:**
- Modify: `internal/graphql/handler.go`

- [ ] **Step 1: Add parseRequestedFields function**

Add at the bottom of handler.go before `writeError`:

```go
// parseNestedFields extracts nested field names from a GraphQL query.
// e.g. "{ cards { id title elements { id component } } }" returns ["elements"]
func parseNestedFields(query string) []string {
	var nested []string
	depth := 0
	inMainBody := false
	current := ""
	i := 0
	runes := []rune(query)

	for i < len(runes) {
		ch := runes[i]
		switch ch {
		case '{':
			depth++
			if depth == 2 {
				inMainBody = true
				current = ""
			} else if depth == 3 && inMainBody {
				// We found a nested object — current holds the field name
				name := strings.TrimSpace(current)
				// Strip args like "elements(limit: 10)"
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
		i++
	}
	return nested
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/flakerim/Construct/infra/graph && go build ./...`
Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add internal/graphql/handler.go
git commit -m "feat: parse nested fields from GraphQL queries"
```

---

### Task 5: GraphQL handler — Wire relations into resolveQuery

Connect the pieces: when resolving a list query, check for relation fields, build JoinSpecs for belongsTo, and batch-load hasMany after the main query.

**Files:**
- Modify: `internal/graphql/handler.go` (update `resolveQuery` method)

- [ ] **Step 1: Add buildRelationSpecs helper**

Add before `resolveQuery`:

```go
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
			Target:    sanitize(f.Target),
			Type:      f.Relation,
		}
		if f.Relation == "belongsTo" {
			spec.FKColumn = sanitize(f.Name) + "_id"
			joins = append(joins, spec)
		} else if f.Relation == "hasMany" {
			// For hasMany, FK is on the target table: e.g. element.card_id
			spec.FKColumn = sanitize(modelName) + "_id"
			hasMany = append(hasMany, spec)
		}
	}
	return
}

func sanitize(s string) string {
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
```

- [ ] **Step 2: Update resolveQuery to use relations**

Replace the list query branch (the `else if strings.HasSuffix(op, "s")` block, lines 239-255) with:

```go
	} else if strings.HasSuffix(op, "s") {
		table := strings.TrimSuffix(op, "s")

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

		if len(joinSpecs) > 0 {
			rows, err = h.engine.FindManyWithJoins(ctx, rctx.schema, table, where, orderBy, limit, offset, joinSpecs)
		} else {
			rows, err = h.engine.FindMany(ctx, rctx.schema, table, where, orderBy, limit, offset)
		}
		if err != nil {
			return nil, err
		}

		// Batch-load hasMany relations
		if len(hasManySpecs) > 0 && len(rows) > 0 {
			// Collect parent IDs
			parentIDs := make([]string, 0, len(rows))
			for _, row := range rows {
				if id, ok := row["id"]; ok {
					parentIDs = append(parentIDs, fmt.Sprint(id))
				}
			}

			for _, spec := range hasManySpecs {
				grouped, err := h.engine.BatchLoadRelated(ctx, rctx.schema, spec.Target, spec.FKColumn, parentIDs)
				if err != nil {
					return nil, err
				}
				// Attach to each parent row
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
```

- [ ] **Step 3: Verify it compiles**

Run: `cd /Users/flakerim/Construct/infra/graph && go build ./...`
Expected: no errors

- [ ] **Step 4: Commit**

```bash
git add internal/graphql/handler.go
git commit -m "feat: wire relation resolution into GraphQL queries"
```

---

### Task 6: Wire relations into single-record queries

The singular query (e.g. `card(id: "xxx") { elements { ... } }`) should also resolve relations.

**Files:**
- Modify: `internal/graphql/handler.go` (update the `else` branch in `resolveQuery`)

- [ ] **Step 1: Update single-record branch**

Replace the else branch (lines 256-270) with:

```go
	} else {
		if err := h.checkAccess(rctx, op, "read"); err != nil {
			return nil, err
		}

		id := extractString(args, vars, "id")
		if id == "" {
			return nil, fmt.Errorf("id required for single record query")
		}
		row, err := h.engine.FindOne(ctx, rctx.schema, op, id)
		if err != nil {
			return nil, err
		}
		if row == nil {
			result[op] = nil
			return result, nil
		}

		// Resolve nested relations for single record
		nestedFields := parseNestedFields(query)
		_, hasManySpecs := h.buildRelationSpecs(rctx.spaceID, op, nestedFields)

		// belongsTo on single record: look for _id fields and fetch
		joinSpecs, _ := h.buildRelationSpecs(rctx.spaceID, op, nestedFields)
		for _, spec := range joinSpecs {
			fkVal := fmt.Sprint(row[spec.FKColumn])
			if fkVal != "" && fkVal != "<nil>" {
				related, err := h.engine.FindOne(ctx, rctx.schema, spec.Target, fkVal)
				if err == nil && related != nil {
					row[spec.FieldName] = related
				}
			}
		}

		// hasMany on single record
		for _, spec := range hasManySpecs {
			parentID := fmt.Sprint(row["id"])
			grouped, err := h.engine.BatchLoadRelated(ctx, rctx.schema, spec.Target, spec.FKColumn, []string{parentID})
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
```

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/flakerim/Construct/infra/graph && go build ./...`
Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add internal/graphql/handler.go
git commit -m "feat: resolve relations on single-record queries"
```

---

### Task 7: Manual integration test

Test the full flow using the canvas space which has `card` and `element` models where element has a `card_id` foreign key.

**Files:** None (manual testing)

- [ ] **Step 1: Build and deploy**

```bash
cd /Users/flakerim/Construct/infra/graph
go build ./...
# Deploy to CapRover or test locally
```

- [ ] **Step 2: Test belongsTo — elements with their card**

Using the GraphQL playground at `graph.lisaos.dev`, set Space=canvas, Project=default:

```graphql
{
  elements(limit: 5) {
    id
    component
    card {
      id
      title
      color
    }
  }
}
```

Expected: Each element includes its parent card as a nested object (from LEFT JOIN).

- [ ] **Step 3: Test hasMany — cards with their elements**

If the canvas manifest defines a hasMany relation on card→elements:

```graphql
{
  cards(limit: 5) {
    id
    title
    elements {
      id
      component
    }
  }
}
```

Expected: Each card includes an array of its child elements (from batch IN query).

- [ ] **Step 4: Test no-relation query still works**

```graphql
{
  cards(limit: 5) {
    id
    title
    color
  }
}
```

Expected: Works exactly as before — no JOINs added when no nested fields requested.

- [ ] **Step 5: Final commit with any fixes**

```bash
git add -A
git commit -m "feat: GraphQL relation resolution with JOINs"
```
