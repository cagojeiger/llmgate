package provider

import (
	"bufio"
	"bytes"
	"io"
	"strings"
)

type SSEReader struct {
	scanner *bufio.Scanner
	done    bool
}

func NewSSEReader(r io.ReadCloser) *SSEReader {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)
	return &SSEReader{scanner: scanner}
}

func (r *SSEReader) Recv() (data []byte, err error) {
	if r.done {
		return nil, io.EOF
	}

	var parts [][]byte
	for r.scanner.Scan() {
		line := r.scanner.Text()
		if line == "" {
			if len(parts) == 0 {
				continue
			}
			payload := bytes.Join(parts, []byte("\n"))
			if string(payload) == "[DONE]" {
				r.done = true
				return nil, io.EOF
			}
			return payload, nil
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
		parts = append(parts, []byte(payload))
	}

	if err := r.scanner.Err(); err != nil {
		return nil, &Error{Kind: KindUpstream, Message: err.Error(), Cause: err}
	}
	return nil, &Error{Kind: KindUpstream, Message: "stream ended without [DONE]"}
}
