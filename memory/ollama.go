package memory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// OllamaEmbedder embeds text via an Ollama server's /api/embed endpoint. The
// expected dimension is fixed at construction (e.g. 768 for nomic-embed-text)
// so a misconfigured model surfaces immediately rather than corrupting recall.
type OllamaEmbedder struct {
	url    string
	model  string
	dim    int
	client *http.Client
}

// NewOllamaEmbedder builds an embedder pointing at an Ollama base URL (no
// trailing /api/embed). dim is the model's output dimension.
func NewOllamaEmbedder(url, model string, dim int) *OllamaEmbedder {
	return &OllamaEmbedder{
		url:    url,
		model:  model,
		dim:    dim,
		client: &http.Client{Timeout: 2 * time.Minute},
	}
}

func (e *OllamaEmbedder) Dim() int { return e.dim }

type ollamaEmbedReq struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type ollamaEmbedResp struct {
	Embeddings [][]float32 `json:"embeddings"`
}

func (e *OllamaEmbedder) Embed(texts []string) ([][]float32, error) {
	body, err := json.Marshal(ollamaEmbedReq{Model: e.model, Input: texts})
	if err != nil {
		return nil, err
	}
	resp, err := e.client.Post(e.url+"/api/embed", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama embed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama embed: status %d", resp.StatusCode)
	}
	var out ollamaEmbedResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("ollama embed decode: %w", err)
	}
	if len(out.Embeddings) != len(texts) {
		return nil, fmt.Errorf("ollama embed: asked %d, got %d embeddings", len(texts), len(out.Embeddings))
	}
	for i, v := range out.Embeddings {
		if len(v) != e.dim {
			return nil, fmt.Errorf("ollama embed: vector %d dim %d, want %d (wrong model?)", i, len(v), e.dim)
		}
	}
	return out.Embeddings, nil
}
