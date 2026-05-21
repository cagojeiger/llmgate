package llmtypes

import (
	"encoding/json"
	"reflect"
	"strings"
)

// unmarshalWithExtra decodes data into target (which must be a pointer
// to a method-less alias of a struct that has an Extra map field) and
// populates Extra with any unrecognized JSON keys. The alias trick
// suppresses recursion into the original type's UnmarshalJSON.
func unmarshalWithExtra(data []byte, target any, known map[string]struct{}) error {
	if err := json.Unmarshal(data, target); err != nil {
		return err
	}
	extra, err := extraFields(data, known)
	if err != nil {
		return err
	}
	if extra == nil {
		return nil
	}
	reflect.ValueOf(target).Elem().FieldByName("Extra").Set(reflect.ValueOf(extra))
	return nil
}

func knownJSONFields(t reflect.Type) map[string]struct{} {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	fields := make(map[string]struct{}, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "" {
			name = field.Name
		}
		fields[name] = struct{}{}
	}
	return fields
}

func extraFields(data []byte, known map[string]struct{}) (map[string]json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	extra := make(map[string]json.RawMessage)
	for key, value := range raw {
		if _, ok := known[key]; ok {
			continue
		}
		extra[key] = value
	}
	if len(extra) == 0 {
		return nil, nil
	}
	return extra, nil
}

func marshalWithExtra(v any, extra map[string]json.RawMessage) ([]byte, error) {
	base, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(extra) == 0 {
		return base, nil
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(base, &raw); err != nil {
		return nil, err
	}
	for key, value := range extra {
		if _, ok := raw[key]; ok {
			continue
		}
		raw[key] = value
	}
	return json.Marshal(raw)
}
