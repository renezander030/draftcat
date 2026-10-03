package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/renezander030/draftcat/internal/config"
	skillsapi "github.com/renezander030/draftcat/internal/skills"
	"gopkg.in/yaml.v3"
)

func TestValidateOutputIntegerAndExactNumbers(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		definition  map[string]interface{}
		wantError   string
	}{
		{"fractional integer", `{"value":3.5}`, map[string]interface{}{"type": "int"}, "expected int"},
		{"integral decimal", `{"value":3.0}`, map[string]interface{}{"type": "int"}, ""},
		{"integral exponent", `{"value":3e2}`, map[string]interface{}{"type": "int"}, ""},
		{"fractional exponent", `{"value":3e-2}`, map[string]interface{}{"type": "int"}, "expected int"},
		{"exact large bound", `{"value":9007199254740993}`, map[string]interface{}{"type": "int", "max": int64(9007199254740992)}, "above max"},
		{"exact decimal bound", `{"value":0.100000000000000001}`, map[string]interface{}{"type": "number", "max": 0.1}, "above max"},
		{"exact large enum", `{"value":9007199254740993}`, map[string]interface{}{"type": "int", "enum": []interface{}{int64(9007199254740993)}}, ""},
		{"large enum neighbor", `{"value":9007199254740992}`, map[string]interface{}{"type": "int", "enum": []interface{}{int64(9007199254740993)}}, "not in allowed set"},
		{"numeric string", `{"value":"3"}`, map[string]interface{}{"type": "number"}, "expected number"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := validateOutput(tc.input, map[string]interface{}{"value": tc.definition})
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("wanted %q, got %v", tc.wantError, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := parsed["value"].(json.Number); !ok {
				t.Fatalf("numeric output lost exact JSON representation: %T", parsed["value"])
			}
			encoded, err := json.Marshal(parsed)
			if err != nil || string(encoded) != tc.input {
				t.Fatalf("numeric token changed: %s, %v", encoded, err)
			}
		})
	}
}

func TestValidateOutputRejectsAmbiguousObjects(t *testing.T) {
	schema := map[string]interface{}{"value": map[string]interface{}{"type": "int"}}
	for _, input := range []string{
		`{"value":1,"value":2}`,
		`{"value":1,"extra":{"secret":"first","secret":"second"}}`,
		`{"value":1} {"value":2}`,
		`null`, `[]`, `1`, `"text"`,
		"```json\n{\"value\":1}\nnot a closing fence",
		"```json\n{\"value\":1}\n```\ntrailing prose",
	} {
		if _, err := validateOutput(input, schema); err == nil {
			t.Errorf("accepted ambiguous/non-object output %q", input)
		}
	}
}

func TestValidateOutputPreservesExtraFieldsAndNumericTokens(t *testing.T) {
	input := `{"value":1,"extra":{"large":9007199254740993}}`
	parsed, err := validateOutput(input, map[string]interface{}{"value": map[string]interface{}{"type": "int"}})
	if err != nil {
		t.Fatal(err)
	}
	extra, ok := parsed["extra"].(map[string]interface{})
	if !ok || extra["large"] != json.Number("9007199254740993") {
		t.Fatalf("extra fields changed: %+v", parsed)
	}
}

func TestValidateOutputCompositeEnumFailsWithoutPanic(t *testing.T) {
	schema := map[string]interface{}{"value": map[string]interface{}{"enum": []interface{}{"safe"}}}
	for _, input := range []string{`{"value":{}}`, `{"value":[]}`} {
		if _, err := validateOutput(input, schema); err == nil {
			t.Errorf("accepted composite enum value %q", input)
		}
	}
	// A malformed configured member is rejected before value comparison.
	bad := map[string]interface{}{"value": map[string]interface{}{"enum": []interface{}{map[string]interface{}{"secret": "hidden"}}}}
	if _, err := validateOutput(`{"value":{}}`, bad); err == nil {
		t.Fatal("accepted composite enum definition")
	}
}

func TestValidateOutputErrorsOmitModelData(t *testing.T) {
	schema := map[string]interface{}{"value": map[string]interface{}{"type": "string", "enum": []interface{}{"safe"}}}
	for _, input := range []string{
		`{"value":"sensitive-customer-data"}`,
		`{"value":"sensitive-customer-data", broken}`,
		`{"sensitive-customer-data":1,"sensitive-customer-data":2}`,
	} {
		_, err := validateOutput(input, schema)
		if err == nil || strings.Contains(err.Error(), "sensitive-customer-data") {
			t.Fatalf("error disclosed model data: %v", err)
		}
	}
}

func TestValidateOutputReportsFieldsDeterministically(t *testing.T) {
	schema := map[string]interface{}{"z": map[string]interface{}{"type": "int"}, "a": map[string]interface{}{"type": "int"}}
	for i := 0; i < 20; i++ {
		_, err := validateOutput(`{}`, schema)
		if err == nil || err.Error() != "missing required field: a" {
			t.Fatalf("unstable error: %v", err)
		}
	}
}

func TestOutputScoreKeepsExactIntegers(t *testing.T) {
	for _, tc := range []struct {
		value interface{}
		want  string
	}{
		{json.Number("3"), "3"}, {json.Number("3.0"), "3"},
		{json.Number("9007199254740993"), "9007199254740993"}, {float64(4), "4"}, {nil, "0"},
	} {
		if got := outputScore(tc.value); got != tc.want {
			t.Errorf("score %v rendered as %q, want %q", tc.value, got, tc.want)
		}
	}
}

func TestValidateOutputUsesExactYAMLStepAndSkillSchemas(t *testing.T) {
	var step config.StepConfig
	var skill skillsapi.SkillDef
	definition := []byte("output_schema:\n  decimal: {type: number, max: 0.100000000000000001}\n  large: {type: int, enum: [9007199254740993]}\n")
	if err := yaml.Unmarshal(definition, &step); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(definition, &skill); err != nil {
		t.Fatal(err)
	}
	for _, schema := range []map[string]interface{}{step.OutputSchema, skill.OutputSchema} {
		if _, err := validateOutput(`{"decimal":0.100000000000000001,"large":9007199254740993}`, schema); err != nil {
			t.Fatalf("exact configured decimals/integers changed: %v", err)
		}
		if _, err := validateOutput(`{"decimal":0.100000000000000002,"large":9007199254740993}`, schema); err == nil {
			t.Fatal("accepted value one decimal unit above exact YAML max")
		}
		if _, err := validateOutput(`{"decimal":0.1,"large":9007199254740992}`, schema); err == nil {
			t.Fatal("accepted rounded neighbor of exact YAML enum")
		}
	}
}
