// Package strictjson validates JSON documents without accepting ambiguous
// duplicate object keys, then decodes them with the standard Go field rules.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

var (
	ErrDuplicateKey = errors.New("JSON object contains a duplicate key")
	ErrTrailingData = errors.New("JSON document contains trailing data")
	ErrTooDeep      = errors.New("JSON document exceeds maximum nesting depth")
)

const maxNestingDepth = 1000

// Validate requires exactly one syntactically valid JSON value and rejects
// duplicate object keys at any nesting depth.
func Validate(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := consumeValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return ErrTrailingData
		}
		return err
	}
	return nil
}

// Decode validates the document, rejects unknown struct fields, and decodes
// the single value into destination.
func Decode(data []byte, destination any) error {
	if err := Validate(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(destination)
}

func consumeValue(decoder *json.Decoder, depth int) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if depth >= maxNestingDepth {
		return ErrTooDeep
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("invalid JSON object key")
			}
			if _, exists := seen[key]; exists {
				return ErrDuplicateKey
			}
			seen[key] = struct{}{}
			if err := consumeValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return errors.New("invalid JSON object")
		}
	case '[':
		for decoder.More() {
			if err := consumeValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return errors.New("invalid JSON array")
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	return nil
}
