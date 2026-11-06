package soniox

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/coder/websocket"
)

type message struct {
	kind    websocket.MessageType
	payload []byte
	err     error
}

func (m message) endOfInput() bool { return m.kind == websocket.MessageText && len(m.payload) == 0 }

func (m message) captureKind() string {
	if m.kind == websocket.MessageBinary {
		return "binary"
	}
	return "text"
}

func validStart(m message) bool {
	if m.kind != websocket.MessageText || len(m.payload) > maxStartBytes || !safeObject(m.payload) {
		return false
	}
	var config map[string]json.RawMessage
	var model, audioFormat string
	return json.Unmarshal(m.payload, &config) == nil &&
		json.Unmarshal(config["model"], &model) == nil && model == Model &&
		json.Unmarshal(config["audio_format"], &audioFormat) == nil && audioFormat != ""
}

func validClientMessage(m message) bool {
	if m.kind == websocket.MessageBinary || m.endOfInput() {
		return true
	}
	if len(m.payload) == 0 || !utf8.Valid(m.payload) {
		return false
	}
	if bytes.HasPrefix(bytes.TrimSpace(m.payload), []byte("{")) {
		if !safeObject(m.payload) {
			return false
		}
		var control struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(m.payload, &control) != nil {
			return false
		}
		switch control.Type {
		case "finalize", "keepalive", "agent_text":
			return true
		default:
			return false
		}
	}
	// The Soniox protocol also accepts standard base64 audio in text messages.
	n, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, bytes.NewReader(m.payload)))
	return err == nil && n > 0
}

func safeObject(payload []byte) bool {
	if !utf8.Valid(payload) || !json.Valid(payload) {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	first, err := dec.Token()
	return err == nil && first == json.Delim('{') && safeValue(dec, first, 0)
}

// Scan every key before archiving, including keys hidden in duplicate objects.
// Decoding into a Go map alone would silently overwrite those objects.
func safeValue(dec *json.Decoder, first json.Token, depth int) bool {
	if depth > 64 {
		return false
	}
	switch first {
	case json.Delim('{'):
		seen := make(map[string]struct{})
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return false
			}
			name := strings.ToLower(key.(string))
			if _, duplicate := seen[name]; duplicate || name == "api_key" {
				return false
			}
			seen[name] = struct{}{}
			value, err := dec.Token()
			if err != nil || !safeValue(dec, value, depth+1) {
				return false
			}
		}
	case json.Delim('['):
		for dec.More() {
			value, err := dec.Token()
			if err != nil || !safeValue(dec, value, depth+1) {
				return false
			}
		}
	default:
		return true
	}
	_, err := dec.Token()
	return err == nil
}

type providerResult struct {
	Finished  bool   `json:"finished"`
	ErrorCode int    `json:"error_code"`
	ErrorType string `json:"error_type"`
}

func (r providerResult) failed() bool { return r.ErrorCode != 0 || r.ErrorType != "" }

func readMessages(ctx context.Context, conn *websocket.Conn, out chan<- message) {
	for {
		kind, payload, err := conn.Read(ctx)
		select {
		case out <- message{kind: kind, payload: payload, err: err}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

func writeMessage(ctx context.Context, conn *websocket.Conn, msg message) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return conn.Write(ctx, msg.kind, msg.payload)
}

func writeError(ctx context.Context, conn *websocket.Conn, status int, reason, detail string) {
	payload, _ := json.Marshal(struct {
		Tokens    []struct{} `json:"tokens"`
		ErrorCode int        `json:"error_code"`
		ErrorType string     `json:"error_type"`
		Message   string     `json:"error_message"`
	}{[]struct{}{}, status, reason, detail})
	_ = writeMessage(ctx, conn, message{kind: websocket.MessageText, payload: payload})
}
