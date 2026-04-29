package provider

import (
	"encoding/json"
	"reflect"
	"strings"
)

var (
	requestJSONFields  = knownJSONFields(reflect.TypeOf(Request{}))
	messageJSONFields  = knownJSONFields(reflect.TypeOf(Message{}))
	responseJSONFields = knownJSONFields(reflect.TypeOf(Response{}))
	usageJSONFields    = knownJSONFields(reflect.TypeOf(Usage{}))
	eventJSONFields    = knownJSONFields(reflect.TypeOf(Event{}))
	deltaJSONFields    = knownJSONFields(reflect.TypeOf(Delta{}))
)

func (r *Request) UnmarshalJSON(data []byte) error {
	type request Request
	var out request
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	extra, err := extraFields(data, requestJSONFields)
	if err != nil {
		return err
	}
	out.Extra = extra
	*r = Request(out)
	return nil
}

func (r Request) MarshalJSON() ([]byte, error) {
	type request Request
	return marshalWithExtra(request(r), r.Extra)
}

func (m *Message) UnmarshalJSON(data []byte) error {
	type message Message
	var out message
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	extra, err := extraFields(data, messageJSONFields)
	if err != nil {
		return err
	}
	out.Extra = extra
	*m = Message(out)
	return nil
}

func (m Message) MarshalJSON() ([]byte, error) {
	type message Message
	return marshalWithExtra(message(m), m.Extra)
}

func (r *Response) UnmarshalJSON(data []byte) error {
	type response Response
	var out response
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	extra, err := extraFields(data, responseJSONFields)
	if err != nil {
		return err
	}
	out.Extra = extra
	*r = Response(out)
	return nil
}

func (r Response) MarshalJSON() ([]byte, error) {
	type response Response
	return marshalWithExtra(response(r), r.Extra)
}

func (u *Usage) UnmarshalJSON(data []byte) error {
	type usage Usage
	var out usage
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	extra, err := extraFields(data, usageJSONFields)
	if err != nil {
		return err
	}
	out.Extra = extra
	*u = Usage(out)
	return nil
}

func (u Usage) MarshalJSON() ([]byte, error) {
	type usage Usage
	return marshalWithExtra(usage(u), u.Extra)
}

func (e *Event) UnmarshalJSON(data []byte) error {
	type event Event
	var out event
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	extra, err := extraFields(data, eventJSONFields)
	if err != nil {
		return err
	}
	out.Extra = extra
	*e = Event(out)
	return nil
}

func (e Event) MarshalJSON() ([]byte, error) {
	type event Event
	return marshalWithExtra(event(e), e.Extra)
}

func (d *Delta) UnmarshalJSON(data []byte) error {
	type delta Delta
	var out delta
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	extra, err := extraFields(data, deltaJSONFields)
	if err != nil {
		return err
	}
	out.Extra = extra
	*d = Delta(out)
	return nil
}

func (d Delta) MarshalJSON() ([]byte, error) {
	type delta Delta
	return marshalWithExtra(delta(d), d.Extra)
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
