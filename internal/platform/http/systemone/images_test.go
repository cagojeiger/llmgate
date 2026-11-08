package systemone

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/klauspost/compress/zstd"

	httpauth "llmgate/internal/platform/http/auth"
)

const (
	imageRequest = `{"model":"jev-1.14.0","state":{"photo":"rectangle-photo"},"questions":{
		"present":{"type":"noul","instructions":"Is there a rectangle in the photo?"},
		"color":{"type":"choice","instructions":"What color is the rectangle?","criteria":{"red":"Red","blue":"Blue"}},
		"brightness":{"type":"score","instructions":"How bright is the photo?","criteria":["Dark","Bright"]},
		"rectangle":{"type":"box","instructions":"Locate the rectangle in the photo."}
	},"assets":{"rectangle-photo":ASSET}}`
	imageAnswer = `{
		"model":"jev-1.14.0",
		"answers":{
			"present":{"type":"noul","noul":0.98},
			"color":{"type":"choice","choice":"red","confidence":0.9,"probabilities":{"red":0.98,"blue":0.02}},
			"brightness":{"type":"score","score":0.8,"confidence":0.9,"legend":{"0":"Dark","1":"Bright"},"probabilities":{"0":0.2,"1":0.8}},
			"rectangle":{"type":"box","box":{"left":0.2,"top":0.3,"right":0.75,"bottom":0.7},"confidence":null}
		},
		"usage":{"input_tokens":100,"output_tokens":26}
	}`
)

func TestSystemOneImageRequestsAndResults(t *testing.T) {
	imageBytes := rectanglePNG(t)
	inlineJSON := strings.Replace(imageRequest, "ASSET", `{"type":"image","format":"png","data_base64":"`+base64.StdEncoding.EncodeToString(imageBytes)+`"}`, 1)
	multipartJSON := strings.Replace(imageRequest, "ASSET", `{"type":"image","part":"photo"}`, 1)
	multipartBody, multipartType := imageForm(t, multipartJSON, imageBytes, "photo")

	for _, tc := range []struct {
		name        string
		body        string
		contentType string
		encoding    string
	}{
		{name: "inline JSON", body: inlineJSON, contentType: "application/json"},
		{name: "SDK multipart", body: multipartBody, contentType: multipartType},
		{name: "gzip multipart", body: multipartBody, contentType: multipartType, encoding: "gzip"},
		{name: "x-gzip JSON", body: inlineJSON, contentType: "application/json", encoding: "x-gzip"},
		{name: "zstd JSON", body: inlineJSON, contentType: "application/json", encoding: "zstd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type received struct {
				headers http.Header
				body    []byte
			}
			calls := make(chan received, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read upstream request: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				select {
				case calls <- received{headers: r.Header, body: raw}:
				default:
					t.Error("duplicate upstream request")
				}
				_, _ = io.WriteString(w, imageAnswer)
			}))
			defer upstream.Close()

			telemetrySink, resultSink := &eventCapture{}, &resultCapture{}
			h := NewHandler(nil, telemetrySink, HandlerConfig{
				APIKey: "provider-key", Endpoint: upstream.URL, ResultSink: resultSink,
			})
			req := systemOneRequest(encodeImageRequest(t, tc.body, tc.encoding), httpauth.ConsumerInfo{Name: "consumer", AllowedAliases: []string{Model}})
			req.Header.Set("Content-Type", tc.contentType)
			if tc.encoding != "" {
				req.Header.Set("Content-Encoding", tc.encoding)
			}
			req.Header.Set("Authorization", "Bearer gateway-key")
			req.Header.Set("TypeSafe-Danger-Zone-Images", "true")
			req.Header.Set("TypeSafe-Danger-Zone-Long-Context", "true")
			req.Header.Set("X-Caller-Secret", "do-not-forward")
			out := httptest.NewRecorder()
			h.ServeHTTP(out, req)

			if out.Code != http.StatusOK || out.Body.String() != imageAnswer {
				t.Fatalf("response = %d %q", out.Code, out.Body.String())
			}
			var sent received
			select {
			case sent = <-calls:
			default:
				t.Fatal("missing upstream request")
			}
			for name, want := range map[string]string{
				"Authorization":                     "Bearer provider-key",
				"TypeSafe-Danger-Zone-Images":       "true",
				"TypeSafe-Danger-Zone-Long-Context": "true",
				"X-Caller-Secret":                   "",
				"Content-Encoding":                  "",
			} {
				if got := sent.headers.Get(name); got != want {
					t.Errorf("upstream header %s = %q, want %q", name, got, want)
				}
			}
			sentType := sent.headers.Get("Content-Type")
			if len(telemetrySink.events) != 2 || len(resultSink.events) != 1 {
				t.Fatalf("got %d telemetry and %d result events", len(telemetrySink.events), len(resultSink.events))
			}
			result := resultSink.events[0]
			if string(result.DecisionResponse) != imageAnswer {
				t.Fatal("result did not preserve raw response")
			}
			if len(result.DecisionRequestHeaders) != 2 {
				t.Fatalf("result headers = %v", result.DecisionRequestHeaders)
			}
			for name, value := range result.DecisionRequestHeaders {
				if (!strings.EqualFold(name, "TypeSafe-Danger-Zone-Images") && !strings.EqualFold(name, "TypeSafe-Danger-Zone-Long-Context")) || value != "true" {
					t.Errorf("unexpected result header %s = %q", name, value)
				}
			}

			archived := []byte(result.DecisionRequest)
			wantJSON := inlineJSON
			if tc.contentType == multipartType {
				wantJSON = multipartJSON
				var payload struct {
					ContentType string `json:"content_type"`
					Body        []byte `json:"body_base64"`
				}
				if err := json.Unmarshal(archived, &payload); err != nil || payload.ContentType != sentType {
					t.Fatalf("invalid archived multipart envelope: content type %q, error %v", payload.ContentType, err)
				}
				archived = payload.Body
				jsonBody, imageBody := readTestImageForm(t, archived, payload.ContentType)
				if !bytes.Equal(imageBody, imageBytes) {
					t.Fatal("multipart image differs from the uploaded bytes")
				}
				assertImageJSON(t, jsonBody, wantJSON)
			} else {
				assertImageJSON(t, archived, wantJSON)
			}
			if !bytes.Equal(archived, sent.body) {
				t.Fatal("archive did not preserve the upstream request")
			}
		})
	}
}

func TestSystemOneRejectsInvalidCompressedRequestsBeforeUpstream(t *testing.T) {
	compressible := strings.Replace(inputBody, `{"ticket":9007199254740993}`, `"`+strings.Repeat("a", 1024)+`"`, 1)
	compressed := encodeImageRequest(t, compressible, "gzip")
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()

	for _, tc := range []struct {
		name     string
		body     string
		encoding string
		limit    int64
		want     int
	}{
		{name: "malformed gzip", body: inputBody, encoding: "gzip", want: http.StatusBadRequest},
		{name: "layered encodings", body: compressed, encoding: "gzip, zstd", want: http.StatusUnsupportedMediaType},
		{name: "encoded body over cap", body: compressed, encoding: "gzip", limit: 32, want: http.StatusRequestEntityTooLarge},
		{name: "decoded body over cap", body: compressed, encoding: "gzip", limit: int64(len(compressed)), want: http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			results := &resultCapture{}
			h := NewHandler(nil, nil, HandlerConfig{APIKey: "provider-key", Endpoint: upstream.URL, MaxRequestBodyBytes: tc.limit, ResultSink: results})
			req := systemOneRequest(tc.body, httpauth.ConsumerInfo{Name: "consumer"})
			req.Header.Set("Content-Encoding", tc.encoding)
			out := httptest.NewRecorder()
			h.ServeHTTP(out, req)
			if out.Code != tc.want || len(results.events) != 0 {
				t.Fatalf("got status %d and %d results, want status %d and no results", out.Code, len(results.events), tc.want)
			}
		})
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("rejected requests reached upstream %d times", got)
	}
}

func TestSystemOneRejectsInvalidMultipartManifestBeforeUpstream(t *testing.T) {
	imageBytes := rectanglePNG(t)
	manifest := strings.Replace(imageRequest, "ASSET", `{"type":"image","part":"photo"}`, 1)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()

	for _, tc := range []struct {
		name      string
		parts     []string
		truncated bool
	}{
		{name: "unreferenced part", parts: []string{"photo", "extra"}},
		{name: "duplicate part", parts: []string{"photo", "photo"}},
		{name: "missing part"},
		{name: "truncated closing boundary", parts: []string{"photo"}, truncated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, contentType := imageForm(t, manifest, imageBytes, tc.parts...)
			if tc.truncated {
				body = strings.TrimSuffix(body, "--\r\n")
			}
			results := &resultCapture{}
			h := NewHandler(nil, nil, HandlerConfig{APIKey: "provider-key", Endpoint: upstream.URL, ResultSink: results})
			req := systemOneRequest(body, httpauth.ConsumerInfo{Name: "consumer"})
			req.Header.Set("Content-Type", contentType)
			req.Header.Set("TypeSafe-Danger-Zone-Images", "true")
			out := httptest.NewRecorder()
			h.ServeHTTP(out, req)
			if out.Code != http.StatusBadRequest || len(results.events) != 0 {
				t.Fatalf("got status %d and %d results, want status 400 and no results", out.Code, len(results.events))
			}
		})
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("rejected multipart requests reached upstream %d times", got)
	}
}

func rectanglePNG(t *testing.T) []byte {
	t.Helper()
	photo := image.NewNRGBA(image.Rect(0, 0, 320, 320))
	for y := 96; y < 224; y++ {
		for x := 64; x < 240; x++ {
			photo.SetNRGBA(x, y, color.NRGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, photo); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func imageForm(t *testing.T, manifest string, imageBytes []byte, imageParts ...string) (body, contentType string) {
	t.Helper()
	var buf bytes.Buffer
	form := multipart.NewWriter(&buf)
	writePart := func(disposition, contentType string, body []byte) {
		t.Helper()
		w, err := form.CreatePart(textproto.MIMEHeader{
			"Content-Disposition": {disposition},
			"Content-Type":        {contentType},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	writePart(`form-data; name="request"`, "application/json", []byte(manifest))
	for _, name := range imageParts {
		writePart(mime.FormatMediaType("form-data", map[string]string{"name": name, "filename": "rectangle.png"}), "image/png", imageBytes)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String(), form.FormDataContentType()
}

func readTestImageForm(t *testing.T, body []byte, contentType string) (manifest, imageBytes []byte) {
	t.Helper()
	media, params, err := mime.ParseMediaType(contentType)
	if err != nil || media != "multipart/form-data" || params["boundary"] == "" {
		t.Fatalf("unexpected multipart content type %q: %v", contentType, err)
	}
	r := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for _, want := range []struct {
		name        string
		contentType string
		output      *[]byte
	}{
		{name: "request", contentType: "application/json", output: &manifest},
		{name: "photo", contentType: "image/png", output: &imageBytes},
	} {
		part, err := r.NextRawPart()
		if err != nil {
			t.Fatal(err)
		}
		if part.FormName() != want.name || part.Header.Get("Content-Type") != want.contentType {
			t.Fatalf("unexpected multipart part %v", part.Header)
		}
		*want.output, err = io.ReadAll(part)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.NextRawPart(); err != io.EOF {
		t.Fatalf("unexpected end of multipart body: %v", err)
	}
	return manifest, imageBytes
}

func encodeImageRequest(t *testing.T, body, encoding string) string {
	t.Helper()
	switch encoding {
	case "":
		return body
	case "gzip", "x-gzip":
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	case "zstd":
		w, err := zstd.NewWriter(nil)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		return string(w.EncodeAll([]byte(body), nil))
	default:
		t.Fatalf("invalid test encoding %q", encoding)
		return ""
	}
}

func assertImageJSON(t *testing.T, got []byte, want string) {
	t.Helper()
	normalize := func(raw []byte) []byte {
		t.Helper()
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		out, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if !bytes.Equal(normalize(got), normalize([]byte(want))) {
		t.Fatal("image request JSON differs from the submitted manifest")
	}
}
