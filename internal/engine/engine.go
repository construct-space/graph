package engine

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

// EventSink receives graph event ids after they are committed to _system.graph_events.
type EventSink func(id int64)

// GraphEvent is the durable change event emitted for Graph mutations.
type GraphEvent struct {
	ID              int64
	SchemaName      string
	TableName       string
	RecordID        string
	Action          string
	Payload         map[string]any
	PreviousPayload map[string]any
	CreatedAt       string
}

// Engine executes dynamic queries against space schemas
type Engine struct {
	db        *gorm.DB
	eventSink EventSink
}

func New(db *gorm.DB) *Engine {
	return &Engine{db: db}
}

// SetEventSink wires low-latency in-process event fanout for realtime streams.
func (e *Engine) SetEventSink(sink EventSink) {
	e.eventSink = sink
}

// quoteIdent wraps a SQL identifier in double quotes, escaping embedded quotes.
// Works on Postgres and SQLite; required for reserved words like "table", "user", "order".
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// TableRef returns the qualified, quoted table name
func TableRef(schemaName, tableName string) string {
	if IsPostgres() {
		return quoteIdent(schemaName) + "." + quoteIdent(tableName)
	}
	// SQLite: use prefix since no schema support
	return quoteIdent(fmt.Sprintf("%s__%s", schemaName, tableName))
}

// TableRefRaw returns the unquoted equivalent of TableRef, for paths that
// talk to metadata APIs (GORM's Migrator.HasTable, information_schema) which
// take the literal identifier and quote it themselves. Feeding them the
// quoted form silently returns "not found" and triggers duplicate-table
// errors on re-publish.
func TableRefRaw(schemaName, tableName string) string {
	if IsPostgres() {
		return schemaName + "." + tableName
	}
	return fmt.Sprintf("%s__%s", schemaName, tableName)
}

// FindMany queries records from a table with optional filters
func (e *Engine) FindMany(ctx context.Context, schemaName, tableName string, where map[string]any, orderBy map[string]string, limit, offset int) ([]map[string]any, error) {
	table := TableRef(schemaName, tableName)
	query := fmt.Sprintf("SELECT * FROM %s", table)

	var conditions []string
	var args []any

	for field, value := range where {
		col := quoteIdent(field)
		// Handle operators: {$gt: 5}, {$in: [...]}
		if m, ok := value.(map[string]any); ok {
			for op, val := range m {
				switch op {
				case "$gt":
					conditions = append(conditions, fmt.Sprintf("%s > ?", col))
				case "$gte":
					conditions = append(conditions, fmt.Sprintf("%s >= ?", col))
				case "$lt":
					conditions = append(conditions, fmt.Sprintf("%s < ?", col))
				case "$lte":
					conditions = append(conditions, fmt.Sprintf("%s <= ?", col))
				case "$ne":
					conditions = append(conditions, fmt.Sprintf("%s != ?", col))
				case "$like":
					conditions = append(conditions, fmt.Sprintf("%s LIKE ?", col))
				case "$in":
					if arr, ok := val.([]any); ok {
						if len(arr) == 0 {
							// Empty $in must match no rows. "IN ()" is a syntax
							// error in Postgres, so emit a always-false sentinel.
							conditions = append(conditions, "1=0")
							continue
						}
						placeholders := make([]string, len(arr))
						for i, v := range arr {
							placeholders[i] = "?"
							args = append(args, v)
						}
						conditions = append(conditions, fmt.Sprintf("%s IN (%s)", col, strings.Join(placeholders, ",")))
						continue // already added args
					}
				case "$null":
					if val == true {
						conditions = append(conditions, fmt.Sprintf("%s IS NULL", col))
					} else {
						conditions = append(conditions, fmt.Sprintf("%s IS NOT NULL", col))
					}
					continue
				}
				args = append(args, val)
			}
		} else {
			conditions = append(conditions, fmt.Sprintf("%s = ?", col))
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
			orders = append(orders, fmt.Sprintf("%s %s", quoteIdent(field), d))
		}
		query += " ORDER BY " + strings.Join(orders, ", ")
	} else {
		query += " ORDER BY created_at DESC"
	}

	// LIMIT + OFFSET
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	} else {
		query += " LIMIT 100" // default
	}
	if offset > 0 {
		query += fmt.Sprintf(" OFFSET %d", offset)
	}

	rows, err := e.db.WithContext(ctx).Raw(query, args...).Rows()
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	return scanRows(rows)
}

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
			selectParts = append(selectParts, fmt.Sprintf("%s.id AS %q", joinAlias, j.FieldName+"_ref"))
		}

		joinClauses = append(joinClauses,
			fmt.Sprintf("LEFT JOIN %s %s ON %s.id = %s.%s", joinTable, joinAlias, joinAlias, alias, quoteIdent(j.FKColumn)))
	}

	query := fmt.Sprintf("SELECT %s FROM %s %s %s",
		strings.Join(selectParts, ", "),
		table, alias,
		strings.Join(joinClauses, " "))

	// WHERE
	var conditions []string
	var args []any
	for field, value := range where {
		col := alias + "." + quoteIdent(field)
		if m, ok := value.(map[string]any); ok {
			for op, val := range m {
				switch op {
				case "$gt":
					conditions = append(conditions, fmt.Sprintf("%s > ?", col))
				case "$gte":
					conditions = append(conditions, fmt.Sprintf("%s >= ?", col))
				case "$lt":
					conditions = append(conditions, fmt.Sprintf("%s < ?", col))
				case "$lte":
					conditions = append(conditions, fmt.Sprintf("%s <= ?", col))
				case "$ne":
					conditions = append(conditions, fmt.Sprintf("%s != ?", col))
				case "$like":
					conditions = append(conditions, fmt.Sprintf("%s LIKE ?", col))
				case "$in":
					if arr, ok := val.([]any); ok {
						if len(arr) == 0 {
							// Empty $in must match no rows. "IN ()" is a syntax
							// error in Postgres, so emit a always-false sentinel.
							conditions = append(conditions, "1=0")
							continue
						}
						placeholders := make([]string, len(arr))
						for i, v := range arr {
							placeholders[i] = "?"
							args = append(args, v)
						}
						conditions = append(conditions, fmt.Sprintf("%s IN (%s)", col, strings.Join(placeholders, ",")))
						continue
					}
				case "$null":
					if val == true {
						conditions = append(conditions, fmt.Sprintf("%s IS NULL", col))
					} else {
						conditions = append(conditions, fmt.Sprintf("%s IS NOT NULL", col))
					}
					continue
				}
				args = append(args, val)
			}
		} else {
			conditions = append(conditions, fmt.Sprintf("%s = ?", col))
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
			orders = append(orders, fmt.Sprintf("%s.%s %s", alias, quoteIdent(field), d))
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

	rows, err := e.db.WithContext(ctx).Raw(query, args...).Rows()
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	return scanRows(rows)
}

// FindOne returns a single record by ID
func (e *Engine) FindOne(ctx context.Context, schemaName, tableName, id string) (map[string]any, error) {
	table := TableRef(schemaName, tableName)
	rows, err := e.db.WithContext(ctx).Raw(fmt.Sprintf("SELECT * FROM %s WHERE id = ?", table), id).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	results, err := scanRows(rows)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, nil
	}
	return results[0], nil
}

// Count returns the count of matching records
func (e *Engine) Count(ctx context.Context, schemaName, tableName string, where map[string]any) (int, error) {
	table := TableRef(schemaName, tableName)
	query := fmt.Sprintf("SELECT COUNT(*) FROM %s", table)

	var args []any
	var conditions []string

	for field, value := range where {
		conditions = append(conditions, fmt.Sprintf("%s = ?", quoteIdent(field)))
		args = append(args, value)
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}

	var count int
	err := e.db.WithContext(ctx).Raw(query, args...).Scan(&count).Error
	return count, err
}

// Create inserts a record and returns it
func (e *Engine) Create(ctx context.Context, schemaName, tableName string, input map[string]any) (map[string]any, error) {
	table := TableRef(schemaName, tableName)

	// Generate UUID for id if not provided
	if _, ok := input["id"]; !ok {
		input["id"] = generateUUID()
	}

	var cols []string
	var placeholders []string
	var args []any

	for k, v := range input {
		cols = append(cols, quoteIdent(k))
		placeholders = append(placeholders, "?")
		args = append(args, v)
	}

	insertSQL := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		table, strings.Join(cols, ","), strings.Join(placeholders, ","))

	if err := e.db.WithContext(ctx).Exec(insertSQL, args...).Error; err != nil {
		return nil, fmt.Errorf("insert: %w", err)
	}

	// Fetch the inserted row
	row, err := e.FindOne(ctx, schemaName, tableName, fmt.Sprint(input["id"]))
	if err != nil {
		return nil, err
	}
	if row != nil {
		if err := e.emitGraphEvent(ctx, schemaName, tableName, "created", row, nil); err != nil {
			return nil, err
		}
	}
	return row, nil
}

// Update modifies a record by ID
func (e *Engine) Update(ctx context.Context, schemaName, tableName, id string, input map[string]any) (map[string]any, error) {
	table := TableRef(schemaName, tableName)
	previous, err := e.FindOne(ctx, schemaName, tableName, id)
	if err != nil {
		return nil, err
	}

	var sets []string
	var args []any

	for k, v := range input {
		sets = append(sets, fmt.Sprintf("%s = ?", quoteIdent(k)))
		args = append(args, v)
	}

	// Add updated_at
	now := nowFunc()
	sets = append(sets, "updated_at = ?")
	args = append(args, now)

	args = append(args, id)
	updateSQL := fmt.Sprintf("UPDATE %s SET %s WHERE id = ?", table, strings.Join(sets, ","))

	if err := e.db.WithContext(ctx).Exec(updateSQL, args...).Error; err != nil {
		return nil, fmt.Errorf("update: %w", err)
	}

	row, err := e.FindOne(ctx, schemaName, tableName, id)
	if err != nil {
		return nil, err
	}
	if row != nil {
		if err := e.emitGraphEvent(ctx, schemaName, tableName, "updated", row, previous); err != nil {
			return nil, err
		}
	}
	return row, nil
}

// Delete removes a record by ID
func (e *Engine) Delete(ctx context.Context, schemaName, tableName, id string) error {
	table := TableRef(schemaName, tableName)
	previous, err := e.FindOne(ctx, schemaName, tableName, id)
	if err != nil {
		return err
	}
	if err := e.db.WithContext(ctx).Exec(fmt.Sprintf("DELETE FROM %s WHERE id = ?", table), id).Error; err != nil {
		return err
	}
	if previous != nil {
		return e.emitGraphEvent(ctx, schemaName, tableName, "deleted", previous, previous)
	}
	return nil
}

func (e *Engine) emitGraphEvent(ctx context.Context, schemaName, tableName, action string, payload, previous map[string]any) error {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal graph event payload: %w", err)
	}
	var previousJSON any
	if previous != nil {
		data, err := json.Marshal(previous)
		if err != nil {
			return fmt.Errorf("marshal previous graph event payload: %w", err)
		}
		previousJSON = string(data)
	}

	recordID := fmt.Sprint(payload["id"])
	eventTable := TableRef("_system", "graph_events")
	var eventID int64

	if IsPostgres() {
		err = e.db.WithContext(ctx).Raw(fmt.Sprintf(`
			INSERT INTO %s (schema_name, table_name, record_id, action, payload, previous_payload)
			VALUES (?, ?, ?, ?, ?::jsonb, ?::jsonb)
			RETURNING id
		`, eventTable), schemaName, tableName, recordID, action, string(payloadJSON), previousJSON).Scan(&eventID).Error
	} else {
		err = e.db.WithContext(ctx).Raw(fmt.Sprintf(`
			INSERT INTO %s (schema_name, table_name, record_id, action, payload, previous_payload)
			VALUES (?, ?, ?, ?, ?, ?)
			RETURNING id
		`, eventTable), schemaName, tableName, recordID, action, string(payloadJSON), previousJSON).Scan(&eventID).Error
	}
	if err != nil {
		return fmt.Errorf("insert graph event: %w", err)
	}

	if IsPostgres() {
		if err := e.db.WithContext(ctx).Exec("SELECT pg_notify('graph_events', ?)", strconv.FormatInt(eventID, 10)).Error; err != nil {
			return fmt.Errorf("notify graph event: %w", err)
		}
	}
	if e.eventSink != nil {
		e.eventSink(eventID)
	}
	return nil
}

// ListGraphEvents returns durable graph events after the given cursor.
func (e *Engine) ListGraphEvents(ctx context.Context, schemaName, tableName string, afterID int64, limit int) ([]GraphEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	eventTable := TableRef("_system", "graph_events")
	rows, err := e.db.WithContext(ctx).Raw(fmt.Sprintf(`
		SELECT id, schema_name, table_name, record_id, action, payload, previous_payload, created_at
		FROM %s
		WHERE id > ? AND schema_name = ? AND table_name = ?
		ORDER BY id ASC
		LIMIT ?
	`, eventTable), afterID, schemaName, tableName, limit).Rows()
	if err != nil {
		return nil, fmt.Errorf("list graph events: %w", err)
	}
	defer rows.Close()

	var events []GraphEvent
	for rows.Next() {
		var ev GraphEvent
		var payloadRaw any
		var previousRaw any
		var createdAt any
		if err := rows.Scan(&ev.ID, &ev.SchemaName, &ev.TableName, &ev.RecordID, &ev.Action, &payloadRaw, &previousRaw, &createdAt); err != nil {
			return nil, err
		}
		ev.Payload = parseEventPayload(payloadRaw)
		ev.PreviousPayload = parseEventPayload(previousRaw)
		ev.CreatedAt = fmt.Sprint(createdAt)
		events = append(events, ev)
	}
	return events, rows.Err()
}

func parseEventPayload(raw any) map[string]any {
	if raw == nil {
		return nil
	}
	var data []byte
	switch v := raw.(type) {
	case []byte:
		data = v
	case string:
		data = []byte(v)
	default:
		data = []byte(fmt.Sprint(v))
	}
	if len(data) == 0 {
		return nil
	}
	var out map[string]any
	if json.Unmarshal(data, &out) != nil {
		return nil
	}
	return out
}

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
		table, quoteIdent(fkColumn), strings.Join(placeholders, ","))

	rows, err := e.db.WithContext(ctx).Raw(query, args...).Rows()
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

// scanRows converts database/sql rows to []map[string]any
func scanRows(rows *sql.Rows) ([]map[string]any, error) {
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	var results []map[string]any
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}

		row := make(map[string]any, len(columns))
		for i, col := range columns {
			val := values[i]
			// Convert []byte to string or parsed JSON
			if b, ok := val.([]byte); ok {
				var parsed any
				if json.Unmarshal(b, &parsed) == nil {
					row[col] = parsed
					continue
				}
				row[col] = string(b)
				continue
			}
			row[col] = val
		}
		results = append(results, row)
	}
	return results, nil
}

// generateUUID creates a v4 UUID without external dependencies
func generateUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 2
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func nowFunc() string {
	// Return current time as ISO string (works for both PG and SQLite)
	return time.Now().UTC().Format("2006-01-02 15:04:05")
}
