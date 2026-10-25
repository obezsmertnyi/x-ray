package vectorstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Reranker scores candidate documents against a query with a cross-encoder
// and returns their indices best-first. It is the second-stage ranker the
// EverMemOS and Zep LoCoMo configurations both rely on (Qwen3-Reranker /
// cross_encoder), and the one ranker MemoryStore.Search was missing
// (docs/todo/graphiti-comparison-refinement.md item #5).
//
// Implementations must be safe for concurrent use.
type Reranker interface {
	// Rerank returns the indices of docs ordered best-first, at most topN of
	// them. An error leaves the caller free to fall back to its own order.
	Rerank(ctx context.Context, query string, docs []string, topN int) ([]int, error)
}

// HTTPReranker talks to a llama.cpp `llama-server --reranking` (or any
// Jina/Cohere-shaped `/v1/rerank` endpoint): POST {model, query, documents,
// top_n} → {results: [{index, relevance_score}]}. It is served the same way
// the embedder is — an in-pod sidecar — and is only ever called on the
// fused search path, never on the lexical fast path, so voice keeps its
// sub-millisecond short-circuit.
type HTTPReranker struct {
	endpoint string
	model    string
	client   *http.Client
	// maxDocChars caps each document sent to the model; cross-encoders have
	// a hard context window (512 tokens for bge-reranker-v2-m3) and
	// llama-server rejects, rather than truncates, oversized inputs.
	maxDocChars int
}

// NewHTTPReranker builds a reranker client. endpoint is the server base URL
// (e.g. http://127.0.0.1:8091); model is passed through and may be "" for
// servers that host a single model.
func NewHTTPReranker(endpoint, model string, maxDocChars int, timeout time.Duration) *HTTPReranker {
	if maxDocChars <= 0 {
		maxDocChars = 1500
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &HTTPReranker{
		endpoint:    strings.TrimRight(endpoint, "/"),
		model:       model,
		client:      &http.Client{Timeout: timeout},
		maxDocChars: maxDocChars,
	}
}

type rerankRequest struct {
	Model     string   `json:"model,omitempty"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n,omitempty"`
}

type rerankResponse struct {
	Results []struct {
		Index          int     `json:"index"`
		RelevanceScore float64 `json:"relevance_score"`
	} `json:"results"`
}

// Rerank implements Reranker.
func (r *HTTPReranker) Rerank(ctx context.Context, query string, docs []string, topN int) ([]int, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	if topN <= 0 || topN > len(docs) {
		topN = len(docs)
	}
	trimmed := make([]string, len(docs))
	for i, d := range docs {
		if len(d) > r.maxDocChars {
			d = d[:r.maxDocChars]
		}
		trimmed[i] = d
	}
	body, err := json.Marshal(rerankRequest{Model: r.model, Query: query, Documents: trimmed, TopN: topN})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint+"/v1/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rerank request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rerank: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out rerankResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("rerank: decode: %w", err)
	}
	// Servers are expected to return best-first, but sort defensively so a
	// caller can rely on the contract regardless of implementation.
	sort.SliceStable(out.Results, func(i, j int) bool { return out.Results[i].RelevanceScore > out.Results[j].RelevanceScore })
	idx := make([]int, 0, topN)
	seen := make(map[int]struct{}, topN)
	for _, res := range out.Results {
		if res.Index < 0 || res.Index >= len(docs) {
			continue
		}
		if _, dup := seen[res.Index]; dup {
			continue
		}
		seen[res.Index] = struct{}{}
		idx = append(idx, res.Index)
		if len(idx) >= topN {
			break
		}
	}
	return idx, nil
}
