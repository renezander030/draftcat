package main

import (
	"fmt"
	"strings"

	"github.com/renezander030/draftcat/internal/outputschema"
)

// enumContains retains the flat schema helper's exact scalar comparisons.
func enumContains(allowed []interface{}, value interface{}) bool {
	return outputschema.EnumContains(allowed, value)
}

func validateOutput(text string, schema map[string]interface{}) (map[string]interface{}, error) {
	if len(schema) == 0 {
		return nil, nil
	}
	if findings := outputschema.Check(schema); len(findings) > 0 {
		return nil, fmt.Errorf("field %s: invalid output schema: %s", findings[0].Field, findings[0].Message)
	}

	cleaned := strings.TrimSpace(text)
	if strings.HasPrefix(cleaned, "```") {
		lines := strings.Split(cleaned, "\n")
		if len(lines) < 3 || strings.TrimSpace(lines[len(lines)-1]) != "```" ||
			(strings.TrimSpace(lines[0]) != "```" && strings.TrimSpace(lines[0]) != "```json") {
			return nil, fmt.Errorf("output has an invalid JSON code fence")
		}
		cleaned = strings.Join(lines[1:len(lines)-1], "\n")
	}

	var parsed map[string]interface{}
	if err := decodeStrictJSON([]byte(cleaned), &parsed); err != nil || parsed == nil {
		// Decoder details can include model-controlled field names. Keep model
		// output out of errors, which flow into logs and operator notifications.
		return nil, fmt.Errorf("output must be one unambiguous JSON object")
	}

	for _, key := range outputschema.Fields(schema) {
		value, exists := parsed[key]
		if !exists {
			return nil, fmt.Errorf("missing required field: %s", key)
		}
		def, ok := schema[key].(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("field %s: invalid output schema definition", key)
		}
		if typeName, hasType := def["type"].(string); hasType {
			if !outputschema.MatchesType(value, typeName) {
				return nil, fmt.Errorf("field %s: expected %s", key, typeName)
			}
			if typeName == "int" || typeName == "number" {
				number, _ := outputschema.Number(value)
				if min, present := def["min"]; present {
					bound, _ := outputschema.Number(min)
					if number.Cmp(bound) < 0 {
						return nil, fmt.Errorf("field %s: value below min", key)
					}
				}
				if max, present := def["max"]; present {
					bound, _ := outputschema.Number(max)
					if number.Cmp(bound) > 0 {
						return nil, fmt.Errorf("field %s: value above max", key)
					}
				}
			}
		}
		if enum, present := def["enum"]; present {
			allowed, ok := enum.([]interface{})
			if !ok {
				return nil, fmt.Errorf("field %s: invalid output schema enum", key)
			}
			if !enumContains(allowed, value) {
				return nil, fmt.Errorf("field %s: value not in allowed set", key)
			}
		}
	}
	return parsed, nil
}

// outputScore formats integer scores without converting through float64.
func outputScore(value interface{}) string {
	if number, ok := outputschema.Number(value); ok {
		if number.IsInt() {
			return number.Num().String()
		}
		return fmt.Sprint(value)
	}
	return "0"
}
