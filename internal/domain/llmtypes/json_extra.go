package llmtypes

import "reflect"

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
