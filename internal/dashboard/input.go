package dashboard

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
)

// encoding/json matches struct fields without regard to case. The browser
// contract uses exact JSON names, and raw-field validation must inspect the
// same keys that decoding consumes (especially for optional/null fields).
func exactInputKeys[T any](body []byte) bool {
	return inputObjectKeysMatch(body, reflect.TypeFor[T]())
}

func inputObjectKeysMatch(body []byte, inputType reflect.Type) bool {
	for inputType.Kind() == reflect.Pointer {
		inputType = inputType.Elem()
	}
	if inputType.Kind() != reflect.Struct {
		return true
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return false
	}
	allowed := make(map[string]reflect.Type, inputType.NumField())
	for i := 0; i < inputType.NumField(); i++ {
		field := inputType.Field(i)
		if !field.IsExported() {
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		allowed[name] = field.Type
	}
	for name, raw := range fields {
		fieldType, ok := allowed[name]
		if !ok {
			return false
		}
		for fieldType.Kind() == reflect.Pointer {
			fieldType = fieldType.Elem()
		}
		if fieldType.Kind() == reflect.Struct && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && !inputObjectKeysMatch(raw, fieldType) {
			return false
		}
	}
	return true
}
