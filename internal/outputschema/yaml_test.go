package outputschema

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSchemaRetainsYAMLNumericDigits(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"0.100000000000000001", "0.100000000000000001"},
		{"9007199254740993", "9007199254740993"},
		{"0x20", "32"}, {"0b100000", "32"}, {"040", "32"}, {"0o40", "32"},
		{"+32", "32"}, {"01.2", "1.2"}, {".5", "0.5"}, {"1.", "1.0"},
		{"1.e2", "1.0e2"}, {"1E+03", "1E+03"}, {"-.5", "-0.5"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			var schema Schema
			if err := yaml.Unmarshal([]byte("value: {type: number, min: "+tc.input+", enum: ["+tc.input+"]}\n"), &schema); err != nil {
				t.Fatal(err)
			}
			def := schema["value"].(map[string]interface{})
			for _, value := range []interface{}{def["min"], def["enum"].([]interface{})[0]} {
				if value != json.Number(tc.want) {
					t.Fatalf("YAML token %s changed to %#v", tc.input, value)
				}
			}
		})
	}
}

func TestSchemaAliasesAndMergesKeepExactNumbers(t *testing.T) {
	input := []byte("first: &bounds {type: number, min: 0.100000000000000001}\nsecond: {<<: *bounds, max: 1e0}\n")
	var schema Schema
	if err := yaml.Unmarshal(input, &schema); err != nil {
		t.Fatal(err)
	}
	second := schema["second"].(map[string]interface{})
	if second["min"] != json.Number("0.100000000000000001") || second["max"] != json.Number("1e0") {
		t.Fatalf("merged bound digits changed: %+v", second)
	}
	if f := Check(schema); len(f) != 0 {
		t.Fatalf("merged scalar contract: %+v", f)
	}
}

func TestSchemaRejectsDuplicateAndNonfiniteYAMLScalars(t *testing.T) {
	for _, input := range []string{
		"value: {type: number, min: .inf}\n",
		"value: {type: number, enum: [.nan]}\n",
		"value: {type: int, min: 1, min: 2}\n",
		"value: {type: int}\nvalue: {type: string}\n",
		"value: &recursive {enum: [*recursive]}\n",
	} {
		var schema Schema
		if err := yaml.Unmarshal([]byte(input), &schema); err == nil {
			t.Errorf("accepted duplicate, nonfinite, or recursive schema: %q", input)
		}
	}
}
