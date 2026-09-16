package openai

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"

	"llmgate/internal/domain/llmtypes"
)

func validateEmbeddings(out *llmtypes.EmbeddingResponse, req *llmtypes.EmbeddingRequest, count int) error {
	if out.Object != "list" || len(out.Data) != count {
		return errors.New("embedding count or object mismatch")
	}
	seen := make([]bool, count)
	dimension := 0
	for _, entry := range out.Data {
		if entry.Object != "embedding" || entry.Index < 0 || entry.Index >= count || seen[entry.Index] {
			return errors.New("invalid embedding index or object")
		}
		seen[entry.Index] = true
		size, err := embeddingSize(entry.Embedding, req.EncodingFormat)
		if err != nil {
			return err
		}
		if size == 0 || (dimension != 0 && dimension != size) || (req.Dimensions != nil && size != *req.Dimensions) {
			return errors.New("invalid embedding dimension")
		}
		dimension = size
	}
	return nil
}

func embeddingSize(raw json.RawMessage, format string) (int, error) {
	if format == "base64" {
		var encoded string
		if err := json.Unmarshal(raw, &encoded); err != nil {
			return 0, errors.New("expected base64 embedding")
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(data)%4 != 0 {
			return 0, errors.New("invalid base64 embedding")
		}
		for offset := 0; offset < len(data); offset += 4 {
			value := float64(math.Float32frombits(binary.LittleEndian.Uint32(data[offset:])))
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return 0, errors.New("non-finite embedding")
			}
		}
		return len(data) / 4, nil
	}
	var vector []*float64
	if err := json.Unmarshal(raw, &vector); err != nil {
		return 0, errors.New("expected float embedding")
	}
	for _, value := range vector {
		if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) {
			return 0, errors.New("null or non-finite embedding")
		}
	}
	return len(vector), nil
}
