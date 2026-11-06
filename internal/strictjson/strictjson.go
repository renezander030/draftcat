// Package strictjson decodes JSON that the gate hashes or decides on: numbers
// keep the spelling they were written with, and a body two readers could read
// differently (a duplicate key, trailing content, unknown fields) is refused.
// The gate endpoints and a rule's definition_file decode through it, so a
// definition hashes the same whichever way it arrives.
package strictjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// MaxDepth bounds how deeply objects and arrays may nest.
const MaxDepth = 128

// Decode preserves number tokens and rejects ambiguous envelopes.
func Decode(body []byte, target interface{}) error {
	tokens := json.NewDecoder(bytes.NewReader(body))
	tokens.UseNumber()
	if err := uniqueValue(tokens, 0); err != nil {
		return err
	}
	if _, err := tokens.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON content")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	return dec.Decode(target)
}

func uniqueValue(dec *json.Decoder, depth int) error {
	if depth > MaxDepth {
		return fmt.Errorf("JSON nesting exceeds %d levels", MaxDepth)
	}
	token, err := dec.Token()
	if err != nil {
		return err
	}
	switch token {
	case json.Delim('{'):
		keys := map[string]bool{}
		for dec.More() {
			token, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok {
				return fmt.Errorf("object key must be a string")
			}
			if keys[key] {
				return fmt.Errorf("duplicate JSON key %q", key)
			}
			keys[key] = true
			if err := uniqueValue(dec, depth+1); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	case json.Delim('['):
		for dec.More() {
			if err := uniqueValue(dec, depth+1); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	}
	return nil
}
