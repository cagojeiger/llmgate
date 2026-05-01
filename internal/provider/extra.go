package provider

import (
	"encoding/json"
	"reflect"
	"strings"
)

var (
	requestJSONFields     = knownJSONFields(reflect.TypeOf(Request{}))
	messageJSONFields     = knownJSONFields(reflect.TypeOf(Message{}))
	responseJSONFields    = knownJSONFields(reflect.TypeOf(Response{}))
	usageJSONFields       = knownJSONFields(reflect.TypeOf(Usage{}))
	eventJSONFields       = knownJSONFields(reflect.TypeOf(Event{}))
	deltaJSONFields       = knownJSONFields(reflect.TypeOf(Delta{}))
	choiceJSONFields      = knownJSONFields(reflect.TypeOf(Choice{}))
	choiceDeltaJSONFields = knownJSONFields(reflect.TypeOf(ChoiceDelta{}))
)

func (r *Request) UnmarshalJSON(b []byte) error {
	type a Request
	return unmarshalWithExtra(b, (*a)(r), requestJSONFields)
}

func (r Request) MarshalJSON() ([]byte, error) {
	type a Request
	return marshalWithExtra(a(r), r.Extra)
}

func (m *Message) UnmarshalJSON(b []byte) error {
	type a Message
	return unmarshalWithExtra(b, (*a)(m), messageJSONFields)
}

func (m Message) MarshalJSON() ([]byte, error) {
	type a Message
	return marshalWithExtra(a(m), m.Extra)
}

func (r *Response) UnmarshalJSON(b []byte) error {
	type a Response
	return unmarshalWithExtra(b, (*a)(r), responseJSONFields)
}

func (r Response) MarshalJSON() ([]byte, error) {
	type a Response
	return marshalWithExtra(a(r), r.Extra)
}

func (u *Usage) UnmarshalJSON(b []byte) error {
	type a Usage
	return unmarshalWithExtra(b, (*a)(u), usageJSONFields)
}

func (u Usage) MarshalJSON() ([]byte, error) {
	type a Usage
	return marshalWithExtra(a(u), u.Extra)
}

func (e *Event) UnmarshalJSON(b []byte) error {
	type a Event
	return unmarshalWithExtra(b, (*a)(e), eventJSONFields)
}

func (e Event) MarshalJSON() ([]byte, error) {
	type a Event
	return marshalWithExtra(a(e), e.Extra)
}

func (d *Delta) UnmarshalJSON(b []byte) error {
	type a Delta
	return unmarshalWithExtra(b, (*a)(d), deltaJSONFields)
}

func (d Delta) MarshalJSON() ([]byte, error) {
	type a Delta
	return marshalWithExtra(a(d), d.Extra)
}

func (c *Choice) UnmarshalJSON(b []byte) error {
	type a Choice
	return unmarshalWithExtra(b, (*a)(c), choiceJSONFields)
}

func (c Choice) MarshalJSON() ([]byte, error) {
	type a Choice
	return marshalWithExtra(a(c), c.Extra)
}

func (c *ChoiceDelta) UnmarshalJSON(b []byte) error {
	type a ChoiceDelta
	return unmarshalWithExtra(b, (*a)(c), choiceDeltaJSONFields)
}

func (c ChoiceDelta) MarshalJSON() ([]byte, error) {
	type a ChoiceDelta
	return marshalWithExtra(a(c), c.Extra)
}

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
