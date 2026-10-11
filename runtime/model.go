// Package runtime implements the standard-library-only AppForges transport and codecs.
package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// Optional preserves absence, a value, and explicit JSON null.
type Optional[T any] struct {
	value     T
	set, null bool
}

func Some[T any](value T) Optional[T] { return Optional[T]{value: value, set: true} }
func Null[T any]() Optional[T]        { return Optional[T]{set: true, null: true} }
func (o Optional[T]) Get() (T, bool)  { return o.value, o.set && !o.null }
func (o Optional[T]) IsSet() bool     { return o.set }
func (o Optional[T]) IsNull() bool    { return o.set && o.null }
func (o Optional[T]) MarshalJSON() ([]byte, error) {
	if !o.set || o.null {
		return []byte("null"), nil
	}
	return json.Marshal(o.value)
}
func (o *Optional[T]) UnmarshalJSON(b []byte) error {
	o.set = true
	o.null = bytes.Equal(bytes.TrimSpace(b), []byte("null"))
	if o.null {
		var z T
		o.value = z
		return nil
	}
	return json.Unmarshal(b, &o.value)
}

type presence interface {
	IsSet() bool
	IsNull() bool
}

// Field describes a wire field. Value is a destination pointer when decoding.
type Field struct {
	Name               string
	Value              any
	Required, Nullable bool
	Enum               []string
}

func nilValue(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice:
		return r.IsNil()
	}
	return false
}
func checkValue(b []byte, f Field) error {
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		if !f.Nullable {
			return fmt.Errorf("%s: null is not allowed", f.Name)
		}
		return nil
	}
	if len(f.Enum) > 0 {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return fmt.Errorf("%s: %w", f.Name, err)
		}
		for _, e := range f.Enum {
			if v == e {
				return nil
			}
		}
		return fmt.Errorf("%s: unknown enum value %q", f.Name, v)
	}
	return nil
}
func MarshalModel(fields ...Field) ([]byte, error) {
	obj := map[string]json.RawMessage{}
	for _, f := range fields {
		if o, ok := f.Value.(presence); ok && !o.IsSet() {
			if f.Required {
				return nil, fmt.Errorf("%s: required field missing", f.Name)
			}
			continue
		}
		b, err := json.Marshal(f.Value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Name, err)
		}
		if err = checkValue(b, f); err != nil {
			return nil, err
		}
		obj[f.Name] = b
	}
	return json.Marshal(obj)
}
func UnmarshalModel(data []byte, fields ...Field) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return fmt.Errorf("expected JSON object")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	for _, f := range fields {
		b, ok := obj[f.Name]
		if !ok {
			if f.Required {
				return fmt.Errorf("%s: required field missing", f.Name)
			}
			v := reflect.ValueOf(f.Value)
			if v.Kind() != reflect.Pointer || v.IsNil() {
				return fmt.Errorf("%s: invalid destination", f.Name)
			}
			v.Elem().SetZero()
			continue
		}
		if err := checkValue(b, f); err != nil {
			return err
		}
		if err := json.Unmarshal(b, f.Value); err != nil {
			return fmt.Errorf("%s: %w", f.Name, err)
		}
	}
	return nil
}

// DecodeUnion selects a concrete typed variant from a closed discriminator.
func DecodeUnion(data []byte, discriminator string, variants map[string]func() any) (any, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	var tag string
	if err := json.Unmarshal(obj[discriminator], &tag); err != nil {
		return nil, fmt.Errorf("missing discriminator %s", discriminator)
	}
	factory, ok := variants[tag]
	if !ok {
		return nil, fmt.Errorf("unknown discriminator %q", tag)
	}
	v := factory()
	if err := json.Unmarshal(data, v); err != nil {
		return nil, err
	}
	return v, nil
}

// Value returns the stored optional value or its zero value.
func Value[T any](value Optional[T]) T { v, _ := value.Get(); return v }

// StringValue extracts a string cursor from required and optional fields.
func StringValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case *string:
		if v != nil {
			return *v
		}
	case Optional[string]:
		return Value(v)
	case Optional[*string]:
		if p := Value(v); p != nil {
			return *p
		}
	}
	return ""
}

// Cursor extracts a wire path from a decoded generated model, including optional nested fields.
func Cursor(model any, path string) string {
	data, err := json.Marshal(model)
	if err != nil {
		return ""
	}
	var obj any
	if json.Unmarshal(data, &obj) != nil {
		return ""
	}
	for _, part := range strings.Split(path, ".") {
		m, ok := obj.(map[string]any)
		if !ok {
			return ""
		}
		obj = m[part]
	}
	v, _ := obj.(string)
	return v
}

// MarshalEnum validates a closed string enum before encoding it.
func MarshalEnum(value string, values []string) ([]byte, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if err := checkValue(b, Field{Name: "value", Enum: values}); err != nil {
		return nil, err
	}
	return b, nil
}
