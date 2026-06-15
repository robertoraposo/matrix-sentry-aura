package memory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// MistralEmbedder embeds text via the Mistral API's OpenAI-style
// POST /v1/embeddings endpoint. The expected dimension is fixed at construction
// (mistral-embed = 1024) so a misconfigured model surfaces immediately rather
// than corrupting recall. The API key is never included in errors or logs.
type MistralEmbedder struct {
	url    string // base URL, no trailing slash (default https://api.mistral.ai)
	model  string
	apiKey string
	dim    int
	client *http.Client
}

// NewMistralEmbedder builds an embedder for the Mistral API. dim is the model's
// output dimension (mistral-embed = 1024).
func NewMistralEmbedder(apiKey, model string, dim int) *MistralEmbedder {
	return &MistralEmbedder{
		url:    "https://api.mistral.ai",
		model:  model,
		apiKey: apiKey,
		dim:    dim,
		client: &http.Client{Timeout: 2 * time.Minute},
	}
}

func (e *MistralEmbedder) Dim() int { return e.dim }

type mistralEmbedReq struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type mistralEmbedResp struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

func (e *MistralEmbedder) Embed(texts []string) ([][]float32, error) {
	body, err := json.Marshal(mistralEmbedReq{Model: e.model, Input: texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, e.url+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mistral embed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Deliberately do NOT include the response body: it can echo the key.
		return nil, fmt.Errorf("mistral embed: status %d", resp.StatusCode)
	}
	var out mistralEmbedResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("mistral embed decode: %w", err)
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("mistral embed: asked %d, got %d embeddings", len(texts), len(out.Data))
	}
	vecs := make([][]float32, len(out.Data))
	for _, d := range out.Data {
		if len(d.Embedding) != e.dim {
			return nil, fmt.Errorf("mistral embed: vector dim %d, want %d (wrong model?)", len(d.Embedding), e.dim)
		}
		if d.Index < 0 || d.Index >= len(vecs) {
			return nil, fmt.Errorf("mistral embed: index %d out of range", d.Index)
		}
		vecs[d.Index] = d.Embedding
	}
	for i, v := range vecs {
		if v == nil {
			return nil, fmt.Errorf("mistral embed: missing embedding for index %d", i)
		}
	}
	return vecs, nil
}
