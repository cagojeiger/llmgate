package llmtypes

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestExtraRoundTripRequest(t *testing.T) {
	input := `{
		"model":"deepseek-v4-flash",
		"messages":[{"role":"user","content":"ping","cache_control":{"type":"ephemeral"}}],
		"max_tokens":32,
		"temperature":0.2,
		"top_p":0.9,
		"stop":["END"],
		"seed":7,
		"user":"u1",
		"stream":true,
		"provider_hint":{"route":"fast"}
	}`

	roundTripEqual[Request](t, input)
}

func TestExtraRoundTripMessage(t *testing.T) {
	input := `{
		"role":"assistant",
		"content":"pong",
		"reasoning_content":"short reason",
		"cache_control":{"type":"ephemeral"}
	}`

	roundTripEqual[Message](t, input)
}

func TestMessageStructuredContentRoundTrip(t *testing.T) {
	input := `{
		"role":"user",
		"content":[
			{"type":"text","text":"what is in this image?"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}
		],
		"cache_control":{"type":"ephemeral"}
	}`

	var msg Message
	if err := json.Unmarshal([]byte(input), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.Content != "" {
		t.Fatalf("Content = %q, want empty text field for structured content", msg.Content)
	}
	if len(msg.ContentRaw) == 0 {
		t.Fatal("ContentRaw is empty, want structured content preserved")
	}

	output, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := decodeNormalizedJSON(t, []byte(input))
	got := decodeNormalizedJSON(t, output)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch\nwant: %s\ngot:  %s", mustCompact(input), output)
	}
}

func TestExtraRoundTripResponse(t *testing.T) {
	input := `{
		"id":"chat-1",
		"object":"chat.completion",
		"created":1710000000,
		"model":"deepseek-v4-flash",
		"system_fingerprint":"fp_1",
		"choices":[{
			"index":0,
			"message":{"role":"assistant","content":"pong","reasoning_content":"because","message_vendor":{"x":1}},
			"finish_reason":"stop",
			"logprobs":{"tokens":[]}
		}],
		"usage":{
			"prompt_tokens":5,
			"completion_tokens":1,
			"total_tokens":6,
			"prompt_cache_hit_tokens":4
		},
		"cost":0.001
	}`

	roundTripEqual[Response](t, input)
}

func TestExtraRoundTripUsage(t *testing.T) {
	input := `{
		"prompt_tokens":5,
		"completion_tokens":1,
		"total_tokens":6,
		"prompt_cache_read_tokens":3,
		"prompt_cache_write_tokens":2
	}`

	roundTripEqual[Usage](t, input)
}

func TestExtraRoundTripEvent(t *testing.T) {
	input := `{
		"id":"chat-1",
		"object":"chat.completion.chunk",
		"created":1710000000,
		"model":"deepseek-v4-flash",
		"system_fingerprint":"fp_1",
		"choices":[{
			"index":0,
			"delta":{"role":"assistant","content":"po","reasoning_content":"thinking","delta_vendor":{"x":1}},
			"finish_reason":"stop",
			"logprobs":{"tokens":[]}
		}],
		"usage":{
			"prompt_tokens":5,
			"completion_tokens":1,
			"total_tokens":6,
			"prompt_cache_hit_tokens":4
		},
		"cost":0.001
	}`

	roundTripEqual[Event](t, input)
}

func TestExtraRoundTripDelta(t *testing.T) {
	input := `{
		"role":"assistant",
		"content":"po",
		"reasoning_content":"thinking",
		"refusal":null,
		"tool_calls":[{"id":"x","type":"function"}]
	}`

	roundTripEqual[Delta](t, input)
}

func TestExtraRoundTripChoice(t *testing.T) {
	input := `{
		"index":0,
		"message":{"role":"assistant","content":"pong"},
		"finish_reason":"stop",
		"logprobs":{"tokens":[]},
		"safety_ratings":[{"category":"hate","probability":"NEGLIGIBLE"}],
		"vendor_choice_meta":{"x":1}
	}`

	roundTripEqual[Choice](t, input)
}

func TestExtraRoundTripChoiceDelta(t *testing.T) {
	input := `{
		"index":0,
		"delta":{"role":"assistant","content":"po"},
		"finish_reason":"stop",
		"logprobs":{"tokens":[]},
		"vendor_chunk_meta":{"y":2}
	}`

	roundTripEqual[ChoiceDelta](t, input)
}

func roundTripEqual[T any](t *testing.T, input string) {
	t.Helper()

	var value T
	if err := json.Unmarshal([]byte(input), &value); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	output, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	want := decodeNormalizedJSON(t, []byte(input))
	got := decodeNormalizedJSON(t, output)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch\nwant: %s\ngot:  %s", mustCompact(input), output)
	}
}

func decodeNormalizedJSON(t *testing.T, data []byte) any {
	t.Helper()

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("decode normalized JSON: %v", err)
	}
	return out
}

func mustCompact(input string) []byte {
	var out bytes.Buffer
	if err := json.Compact(&out, []byte(input)); err != nil {
		return []byte(input)
	}
	return out.Bytes()
}
