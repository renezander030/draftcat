package outputschema

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestCheckRejectsUncheckedDefinitions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		definition interface{}
		message    string
	}{
		{"non-map", "int", "definition must be a map"},
		{"empty", map[string]interface{}{}, "missing type or enum"},
		{"unsupported type", map[string]interface{}{"type": "array"}, "type must be"},
		{"malformed type", map[string]interface{}{"type": 1}, "type must be"},
		{"unsupported constraint", map[string]interface{}{"type": "int", "minimum": 1}, "unsupported constraint"},
		{"wrong bound type", map[string]interface{}{"type": "string", "min": 1}, "min/max require"},
		{"string bound", map[string]interface{}{"type": "int", "min": "1"}, "min must be"},
		{"infinite bound", map[string]interface{}{"type": "number", "max": math.Inf(1)}, "max must be"},
		{"inverted bounds", map[string]interface{}{"type": "int", "min": 2, "max": 1}, "min exceeds max"},
		{"non-list enum", map[string]interface{}{"enum": "safe"}, "enum must be"},
		{"empty enum", map[string]interface{}{"enum": []interface{}{}}, "enum must be"},
		{"composite enum", map[string]interface{}{"enum": []interface{}{[]interface{}{1}}}, "finite scalar"},
		{"inconsistent enum", map[string]interface{}{"type": "int", "enum": []interface{}{1.5}}, "does not match"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			findings := Check(map[string]interface{}{"value": tc.definition})
			for _, f := range findings {
				if strings.Contains(f.Message, tc.message) {
					return
				}
			}
			t.Fatalf("wanted %q, got %+v", tc.message, findings)
		})
	}
}

func TestCheckAcceptsFlatScalarContract(t *testing.T) {
	schema := map[string]interface{}{
		"score":  map[string]interface{}{"type": "int", "min": 1, "max": 5, "enum": []interface{}{1, 3, 5}},
		"number": map[string]interface{}{"type": "number", "min": 0.1, "max": 1},
		"choice": map[string]interface{}{"enum": []interface{}{"yes", false, nil}},
	}
	if f := Check(schema); len(f) != 0 {
		t.Fatalf("valid flat schema: %+v", f)
	}
}

func TestExactNumberWorkIsBounded(t *testing.T) {
	for _, value := range []interface{}{json.Number("1e999999999"), json.Number(strings.Repeat("9", 4097)), json.Number("1/2"), "1", math.NaN(), math.Inf(1)} {
		if _, ok := Number(value); ok {
			t.Errorf("accepted invalid or excessive numeric token %v", value)
		}
	}
	if n, ok := Number(json.Number("9007199254740993")); !ok || n.Num().String() != "9007199254740993" {
		t.Fatalf("large integer rounded: %v", n)
	}
}
