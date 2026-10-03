package outputschema

import (
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Schema is the existing flat field map. Its YAML decoder retains numeric
// constraint and enum digits instead of converting decimal scalars to float64.
type Schema map[string]interface{}

func (schema *Schema) UnmarshalYAML(node *yaml.Node) error {
	value, err := decodeYAMLValue(node, 0)
	if err != nil {
		return err
	}
	if value == nil {
		*schema = nil
		return nil
	}
	fields, ok := value.(map[string]interface{})
	if !ok {
		return fmt.Errorf("output_schema must be a field map")
	}
	*schema = Schema(fields)
	return nil
}

func decodeYAMLValue(node *yaml.Node, depth int) (interface{}, error) {
	if depth > 128 {
		return nil, fmt.Errorf("output_schema YAML nesting exceeds 128 levels")
	}
	switch node.Kind {
	case yaml.AliasNode:
		if node.Alias == nil {
			return nil, fmt.Errorf("invalid output_schema YAML alias")
		}
		return decodeYAMLValue(node.Alias, depth+1)
	case yaml.MappingNode:
		result := map[string]interface{}{}
		seen := map[string]bool{}
		var merges []*yaml.Node
		for i := 0; i < len(node.Content); i += 2 {
			keyNode := node.Content[i]
			var key string
			if err := keyNode.Decode(&key); err != nil {
				return nil, fmt.Errorf("output_schema keys must be strings")
			}
			if seen[key] {
				return nil, fmt.Errorf("duplicate output_schema YAML key")
			}
			seen[key] = true
			if keyNode.Tag == "!!merge" {
				merges = append(merges, node.Content[i+1])
				continue
			}
			value, err := decodeYAMLValue(node.Content[i+1], depth+1)
			if err != nil {
				return nil, err
			}
			result[key] = value
		}
		for _, merge := range merges {
			value, err := decodeYAMLValue(merge, depth+1)
			if err != nil {
				return nil, err
			}
			members, sequence := value.([]interface{})
			if !sequence {
				members = []interface{}{value}
			}
			for _, member := range members {
				fields, ok := member.(map[string]interface{})
				if !ok {
					return nil, fmt.Errorf("output_schema YAML merge must contain maps")
				}
				for key, value := range fields {
					if _, present := result[key]; !present {
						result[key] = value
					}
				}
			}
		}
		return result, nil
	case yaml.SequenceNode:
		result := make([]interface{}, 0, len(node.Content))
		for _, child := range node.Content {
			value, err := decodeYAMLValue(child, depth+1)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}
		return result, nil
	case yaml.ScalarNode:
		if node.Tag == "!!int" || node.Tag == "!!float" {
			return yamlNumber(node)
		}
		var value interface{}
		if err := node.Decode(&value); err != nil {
			return nil, fmt.Errorf("invalid output_schema YAML scalar")
		}
		return value, nil
	default:
		return nil, fmt.Errorf("invalid output_schema YAML value")
	}
}

var yamlDecimal = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

func yamlNumber(node *yaml.Node) (json.Number, error) {
	token := strings.ReplaceAll(node.Value, "_", "")
	if len(token) > 4096 {
		return "", fmt.Errorf("output_schema numeric scalar exceeds 4096 characters")
	}
	if node.Tag == "!!int" {
		// Base zero preserves YAML's hexadecimal, binary, and octal integers.
		number, ok := new(big.Int).SetString(token, 0)
		if !ok {
			return "", fmt.Errorf("invalid output_schema integer")
		}
		token = number.String()
	} else {
		if !yamlDecimal.MatchString(token) {
			return "", fmt.Errorf("output_schema numbers must be finite decimals")
		}
		token = strings.TrimPrefix(token, "+")
		sign := ""
		if strings.HasPrefix(token, "-") {
			sign = "-"
			token = token[1:]
		}
		exponent := ""
		if pos := strings.IndexAny(token, "eE"); pos >= 0 {
			exponent = token[pos:]
			token = token[:pos]
		}
		whole, fraction, decimal := strings.Cut(token, ".")
		whole = strings.TrimLeft(whole, "0")
		if whole == "" {
			whole = "0"
		}
		if decimal {
			if fraction == "" {
				fraction = "0"
			}
			token = whole + "." + fraction
		} else {
			token = whole
		}
		token = sign + token + exponent
	}
	value := json.Number(token)
	if _, ok := Number(value); !ok {
		return "", fmt.Errorf("invalid or excessive output_schema numeric scalar")
	}
	return value, nil
}
