package schema

import (
	"strings"
	"testing"
)

func TestFieldToColumn_EscapesStringDefaults(t *testing.T) {
	r := newTestRegistry(t)

	col := r.fieldToColumn(FieldDef{Name: "publisher", Type: "string", Default: "O'Reilly"}, "s_books")

	if !strings.Contains(col, "DEFAULT 'O''Reilly'") {
		t.Fatalf("expected SQL string literal escaping, got %q", col)
	}
}

func TestFieldToColumn_RelationColumnDoesNotInlineForeignKey(t *testing.T) {
	r := newTestRegistry(t)

	col := r.fieldToColumn(FieldDef{Name: "parent", Type: "relation", Relation: "belongsTo", Target: "folder"}, "s_drive")

	if !strings.Contains(col, `"parent_id"`) {
		t.Fatalf("expected relation FK column name, got %q", col)
	}
	if strings.Contains(strings.ToUpper(col), "REFERENCES") {
		t.Fatalf("relation columns must not depend on target table creation order, got %q", col)
	}
}
