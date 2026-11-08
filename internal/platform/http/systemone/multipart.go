package systemone

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
)

func readMultipart(body []byte, boundary string) ([]byte, *multipart.Reader, error) {
	// Check the same boundary rules used when rebuilding the authorized body.
	if err := multipart.NewWriter(io.Discard).SetBoundary(boundary); err != nil {
		return nil, nil, err
	}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	request, err := reader.NextRawPart()
	if err != nil {
		return nil, nil, fmt.Errorf("read request part: %w", err)
	}
	if name, err := canonicalMultipartPartName(request.Header); err != nil || name != "request" {
		return nil, nil, errors.New("first multipart part must be named request")
	}
	jsonBody, err := io.ReadAll(request)
	if err != nil {
		return nil, nil, fmt.Errorf("read request part: %w", err)
	}

	return jsonBody, reader, nil
}

func canonicalMultipartPartName(header textproto.MIMEHeader) (string, error) {
	disposition := header.Values("Content-Disposition")
	if len(disposition) != 1 || len(header.Values("Content-Transfer-Encoding")) != 0 {
		return "", errors.New("invalid multipart part headers")
	}
	kind, params, err := mime.ParseMediaType(disposition[0])
	if err != nil || kind != "form-data" || params["name"] == "" {
		return "", errors.New("multipart parts must have a form-data name")
	}
	// Remove alternate encodings of the same name before forwarding. The
	// gateway and TypeSafe must agree on which part is named request.
	header.Set("Content-Disposition", mime.FormatMediaType(kind, params))
	return params["name"], nil
}

func encodeMultipart(boundary string, canonicalJSON []byte, assets json.RawMessage, reader *multipart.Reader) ([]byte, error) {
	expected, err := multipartAssetNames(assets)
	if err != nil {
		return nil, err
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.SetBoundary(boundary); err != nil {
		return nil, err
	}
	part, err := writer.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="request"`},
		"Content-Type":        {"application/json"},
	})
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(canonicalJSON); err != nil {
		return nil, err
	}
	copyBuffer := make([]byte, 32<<10)
	for {
		image, err := reader.NextRawPart()
		if err == io.EOF { //nolint:errorlint // a wrapped EOF means the closing boundary was missing
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read image part: %w", err)
		}
		name, err := canonicalMultipartPartName(image.Header)
		if err != nil {
			return nil, err
		}
		if _, ok := expected[name]; !ok {
			return nil, errors.New("unexpected or repeated multipart image part")
		}
		delete(expected, name)

		header := textproto.MIMEHeader{"Content-Disposition": image.Header.Values("Content-Disposition")}
		if values := image.Header.Values("Content-Type"); len(values) > 1 {
			return nil, errors.New("multipart image part has multiple content types")
		} else if len(values) == 1 {
			header["Content-Type"] = values
		}
		part, err := writer.CreatePart(header)
		if err != nil {
			return nil, err
		}
		if _, err := io.CopyBuffer(part, image, copyBuffer); err != nil {
			return nil, fmt.Errorf("read image part: %w", err)
		}
	}
	if len(expected) != 0 {
		return nil, errors.New("multipart image part is missing")
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return body.Bytes(), nil
}

func multipartAssetNames(assets json.RawMessage) (map[string]struct{}, error) {
	var entries map[string]struct {
		Part *string `json:"part"`
	}
	if len(assets) != 0 {
		if err := json.Unmarshal(assets, &entries); err != nil {
			return nil, fmt.Errorf("read multipart assets: %w", err)
		}
	}
	names := make(map[string]struct{})
	for _, asset := range entries {
		if asset.Part == nil {
			continue
		}
		name := *asset.Part
		if _, exists := names[name]; name == "" || name == "request" || exists {
			return nil, errors.New("multipart image references must be unique and name an image part")
		}
		names[name] = struct{}{}
	}
	return names, nil
}
