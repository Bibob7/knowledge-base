// Package embed is a client for OpenAI-compatible /embeddings endpoints
// (OpenAI, Ollama, LM Studio, vLLM, ...).
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	baseURL, model, apiKey string
	dim                    int
	http                   *http.Client
}

func New(baseURL, model, apiKey string, dim int) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		apiKey:  apiKey,
		dim:     dim,
		http:    &http.Client{Timeout: 120 * time.Second},
	}
}

const batchSize = 32

// Embed returns one vector per input text (same order).
func (c *Client) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	out := make([][]float32, 0, len(inputs))
	for i := 0; i < len(inputs); i += batchSize {
		end := min(i+batchSize, len(inputs))
		vecs, err := c.embedBatch(ctx, inputs[i:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

func (c *Client) embedBatch(ctx context.Context, inputs []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{"model": c.model, "input": inputs})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding API status %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var parsed struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("embedding response: %w", err)
	}
	if len(parsed.Data) != len(inputs) {
		return nil, fmt.Errorf("embedding API returned %d vectors for %d inputs", len(parsed.Data), len(inputs))
	}
	vecs := make([][]float32, len(inputs))
	for _, d := range parsed.Data {
		if d.Index < 0 || d.Index >= len(vecs) {
			return nil, fmt.Errorf("invalid index %d in embedding response", d.Index)
		}
		if len(d.Embedding) != c.dim {
			return nil, fmt.Errorf("model %q returns dimension %d, but EMBEDDING_DIM=%d is configured",
				c.model, len(d.Embedding), c.dim)
		}
		vecs[d.Index] = d.Embedding
	}
	return vecs, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
