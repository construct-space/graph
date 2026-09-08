package graphql

import (
	"testing"

	"construct-graph/internal/schema"
)

func TestNormalizeJSONInputForModel_StringifiesCompositeValues(t *testing.T) {
	model := schema.ModelDef{
		Name: "note",
		Fields: []schema.FieldDef{
			{Name: "title", Type: "string"},
			{Name: "labels", Type: "json"},
		},
	}
	input := map[string]any{
		"title":  "hello",
		"labels": []any{"bug", "sdk"},
	}

	if err := normalizeJSONInputForModel(model, input); err != nil {
		t.Fatalf("normalize json input: %v", err)
	}

	if got := input["labels"]; got != `["bug","sdk"]` {
		t.Fatalf("expected JSON string for labels, got %#v", got)
	}
	if got := input["title"]; got != "hello" {
		t.Fatalf("expected non-json field to be unchanged, got %#v", got)
	}
}

func TestNormalizeJSONRecordForModel_ParsesJSONStringValues(t *testing.T) {
	model := schema.ModelDef{
		Name: "note",
		Fields: []schema.FieldDef{
			{Name: "labels", Type: "json"},
		},
	}
	row := map[string]any{
		"id":     "note-1",
		"labels": `["bug","sdk"]`,
	}

	normalizeJSONRecordForModel(model, row)

	labels, ok := row["labels"].([]any)
	if !ok {
		t.Fatalf("expected labels to be parsed as []any, got %#v", row["labels"])
	}
	if len(labels) != 2 || labels[0] != "bug" || labels[1] != "sdk" {
		t.Fatalf("unexpected parsed labels: %#v", labels)
	}
}
