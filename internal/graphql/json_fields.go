package graphql

import (
	"encoding/json"

	"construct-graph/internal/schema"
)

func normalizeJSONInputForModel(model schema.ModelDef, input map[string]any) error {
	if input == nil {
		return nil
	}
	for _, f := range model.Fields {
		if f.Type != "json" {
			continue
		}
		value, ok := input[f.Name]
		if !ok || value == nil {
			continue
		}
		normalized, err := normalizeJSONValueForWrite(value)
		if err != nil {
			return err
		}
		input[f.Name] = normalized
	}
	return nil
}

func normalizeJSONValueForWrite(value any) (any, error) {
	if s, ok := value.(string); ok {
		var parsed any
		if json.Unmarshal([]byte(s), &parsed) == nil {
			return s, nil
		}
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return string(data), nil
}

func normalizeJSONRecordForModel(model schema.ModelDef, row map[string]any) {
	if row == nil {
		return
	}
	for _, f := range model.Fields {
		if f.Type != "json" {
			continue
		}
		switch value := row[f.Name].(type) {
		case string:
			var parsed any
			if json.Unmarshal([]byte(value), &parsed) == nil {
				row[f.Name] = parsed
			}
		case []byte:
			var parsed any
			if json.Unmarshal(value, &parsed) == nil {
				row[f.Name] = parsed
			}
		}
	}
}

func normalizeJSONRecordsForModel(model schema.ModelDef, rows []map[string]any) {
	for _, row := range rows {
		normalizeJSONRecordForModel(model, row)
	}
}
