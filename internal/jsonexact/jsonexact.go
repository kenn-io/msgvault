// Package jsonexact validates that JSON object keys use the exact spellings
// declared by a Go type's json tags.
package jsonexact

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// Validate rejects duplicate, unknown, or case-variant object keys recursively.
// Raw JSON values are opaque. Value type validation remains the responsibility
// of the caller's typed decoder.
func Validate(data []byte, target any) error {
	targetType := reflect.TypeOf(target)
	if targetType == nil {
		return errors.New("JSON exact-key target must not be nil")
	}
	if err := validateUniqueMembers(data); err != nil {
		return err
	}
	return validate(data, targetType, "$", map[reflect.Type]map[string]reflect.Type{})
}

func validateUniqueMembers(data []byte) error {
	decoder := jsontext.NewDecoder(bytes.NewReader(data))
	if _, err := decoder.ReadValue(); err != nil {
		return fmt.Errorf("decode JSON value: %w", err)
	}
	if _, err := decoder.ReadValue(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple top-level JSON values")
		}
		return fmt.Errorf("decode trailing JSON value: %w", err)
	}
	return nil
}

func validate(
	data []byte, targetType reflect.Type, path string,
	fieldCache map[reflect.Type]map[string]reflect.Type,
) error {
	for targetType.Kind() == reflect.Pointer {
		targetType = targetType.Elem()
	}
	if targetType == reflect.TypeFor[jsontext.Value]() || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}

	switch targetType.Kind() {
	case reflect.Struct:
		var object map[string]jsontext.Value
		if err := json.Unmarshal(data, &object); err != nil {
			return fmt.Errorf("decode object at %s: %w", path, err)
		}
		fields := jsonFields(targetType, fieldCache)
		for name, value := range object {
			fieldType, ok := fields[name]
			if !ok {
				return fmt.Errorf("unknown field %q at %s", name, path)
			}
			if err := validate(value, fieldType, path+"."+name, fieldCache); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		var items []jsontext.Value
		if err := json.Unmarshal(data, &items); err != nil {
			return fmt.Errorf("decode array at %s: %w", path, err)
		}
		for i, item := range items {
			if err := validate(item, targetType.Elem(), fmt.Sprintf("%s[%d]", path, i), fieldCache); err != nil {
				return err
			}
		}
	default:
		// Scalar values have no object keys. The caller's typed decoder
		// validates their JSON representation and Go type.
	}
	return nil
}

func jsonFields(
	targetType reflect.Type, cache map[reflect.Type]map[string]reflect.Type,
) map[string]reflect.Type {
	if fields, ok := cache[targetType]; ok {
		return fields
	}
	fields := make(map[string]reflect.Type, targetType.NumField())
	for field := range targetType.Fields() {
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		switch name {
		case "-":
			continue
		case "":
			name = field.Name
		}
		fields[name] = field.Type
	}
	cache[targetType] = fields
	return fields
}
