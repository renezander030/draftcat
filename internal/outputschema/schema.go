// Package outputschema validates Draftcat's flat output schema definitions and
// compares numeric values without rounding their decimal representation.
package outputschema

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
)

// Finding identifies an invalid field definition. Findings are sorted by field
// and constraint so startup and runtime report the same first failure.
type Finding struct {
	Field   string
	Message string
}

// Check validates the existing flat field schema: every declared field is
// required; type, numeric min/max, and scalar enum are its supported constraints.
func Check(schema map[string]interface{}) []Finding {
	var findings []Finding
	for _, field := range Fields(schema) {
		add := func(format string, args ...interface{}) {
			findings = append(findings, Finding{field, fmt.Sprintf(format, args...)})
		}
		def, ok := schema[field].(map[string]interface{})
		if !ok {
			add("definition must be a map")
			continue
		}
		for _, key := range Fields(def) {
			if key != "type" && key != "min" && key != "max" && key != "enum" {
				add("unsupported constraint %q", key)
			}
		}
		typeName := ""
		if raw, present := def["type"]; present {
			var valid bool
			typeName, valid = raw.(string)
			if !valid || (typeName != "int" && typeName != "number" && typeName != "bool" && typeName != "string") {
				add("type must be int, number, bool, or string")
			}
		} else if _, present := def["enum"]; !present {
			add("missing type or enum")
		}
		min, hasMin := def["min"]
		max, hasMax := def["max"]
		if hasMin || hasMax {
			if typeName != "int" && typeName != "number" {
				add("min/max require an int or number type")
			}
			minNum, minOK := Number(min)
			maxNum, maxOK := Number(max)
			if hasMin && !minOK {
				add("min must be a finite number")
			}
			if hasMax && !maxOK {
				add("max must be a finite number")
			}
			if hasMin && hasMax && minOK && maxOK && minNum.Cmp(maxNum) > 0 {
				add("min exceeds max")
			}
		}
		if raw, present := def["enum"]; present {
			allowed, valid := raw.([]interface{})
			if !valid || len(allowed) == 0 {
				add("enum must be a nonempty list of scalar values")
				continue
			}
			for i, value := range allowed {
				if !Scalar(value) {
					add("enum[%d] must be a finite scalar value", i)
				} else if typeName != "" && !MatchesType(value, typeName) {
					add("enum[%d] does not match the declared type", i)
				}
			}
		}
	}
	return findings
}

// Fields returns map keys in a stable order.
func Fields(fields map[string]interface{}) []string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Number accepts numeric values from YAML and JSON, retaining all integer and
// json.Number digits. Bounds on token size and exponent bound rational work.
func Number(value interface{}) (*big.Rat, bool) {
	var token string
	switch n := value.(type) {
	case json.Number:
		token = n.String()
	case int:
		token = strconv.FormatInt(int64(n), 10)
	case int8:
		token = strconv.FormatInt(int64(n), 10)
	case int16:
		token = strconv.FormatInt(int64(n), 10)
	case int32:
		token = strconv.FormatInt(int64(n), 10)
	case int64:
		token = strconv.FormatInt(n, 10)
	case uint:
		token = strconv.FormatUint(uint64(n), 10)
	case uint8:
		token = strconv.FormatUint(uint64(n), 10)
	case uint16:
		token = strconv.FormatUint(uint64(n), 10)
	case uint32:
		token = strconv.FormatUint(uint64(n), 10)
	case uint64:
		token = strconv.FormatUint(n, 10)
	case float32:
		if math.IsNaN(float64(n)) || math.IsInf(float64(n), 0) {
			return nil, false
		}
		token = strconv.FormatFloat(float64(n), 'g', -1, 32)
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, false
		}
		token = strconv.FormatFloat(n, 'g', -1, 64)
	default:
		return nil, false
	}
	if len(token) == 0 || len(token) > 4096 || !json.Valid([]byte(token)) {
		return nil, false
	}
	// json.Valid also accepts literals and containers. Numeric inputs must
	// start with a sign or digit before big.Rat sees them.
	if token[0] != '-' && (token[0] < '0' || token[0] > '9') {
		return nil, false
	}
	if pos := strings.IndexAny(token, "eE"); pos >= 0 {
		exponent, err := strconv.Atoi(token[pos+1:])
		if err != nil || exponent < -4096 || exponent > 4096 {
			return nil, false
		}
	}
	return new(big.Rat).SetString(token)
}

// MatchesType treats 1.0 and 1e3 as integers by value, and 1.5 as fractional.
func MatchesType(value interface{}, typeName string) bool {
	switch typeName {
	case "int", "number":
		number, ok := Number(value)
		return ok && (typeName == "number" || number.IsInt())
	case "bool":
		_, ok := value.(bool)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	}
	return false
}

func Scalar(value interface{}) bool {
	if value == nil {
		return true
	}
	switch value.(type) {
	case bool, string:
		return true
	default:
		_, ok := Number(value)
		return ok
	}
}

// EnumContains compares only scalar values, so a model returning an object or
// array where an enum is expected is a validation failure, never a panic.
func EnumContains(allowed []interface{}, value interface{}) bool {
	if number, ok := Number(value); ok {
		for _, member := range allowed {
			if other, ok := Number(member); ok && number.Cmp(other) == 0 {
				return true
			}
		}
		return false
	}
	for _, member := range allowed {
		switch v := value.(type) {
		case nil:
			if member == nil {
				return true
			}
		case string:
			if other, ok := member.(string); ok && v == other {
				return true
			}
		case bool:
			if other, ok := member.(bool); ok && v == other {
				return true
			}
		}
	}
	return false
}
