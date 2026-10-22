// handler_embeddings.go OpenAI Embeddings 兼容端点：POST /v1/embeddings。
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

type embeddingRequest struct {
	Model          string `json:"model"`
	Input          any    `json:"input"`
	User           string `json:"user,omitempty"`
	EncodingFormat string `json:"encoding_format,omitempty"`
	Dimensions     int    `json:"dimensions,omitempty"`
}

type embeddingData struct {
	Object    string `json:"object"`
	Embedding any    `json:"embedding"`
	Index     int    `json:"index"`
}

type embeddingUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type embeddingResponse struct {
	Object string          `json:"object"`
	Data   []embeddingData `json:"data"`
	Model  string          `json:"model"`
	Usage  embeddingUsage  `json:"usage"`
}

func (h *Handler) embeddings(w http.ResponseWriter, r *http.Request) {
	body, err := ReadBodyLimited(r)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}

	var req embeddingRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return
	}

	if req.Model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "missing required field: model")
		return
	}

	inputs, err := parseEmbeddingInputs(req.Input)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if len(inputs) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "input must not be empty")
		return
	}

	kind, upstreamModel, _ := provider.SplitModelID(req.Model)
	rt := h.getRuntime(kind)
	if rt == nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_model", fmt.Sprintf("unknown channel for model: %q", req.Model))
		return
	}

	if rt.Pool == nil || rt.Upstream == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "channel_unavailable", fmt.Sprintf("channel %s not available", kind))
		return
	}

	acct := rt.Pool.PickForModel(upstreamModel)
	if acct == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", fmt.Sprintf("no healthy account for channel %s", kind))
		return
	}

	embedder, ok := rt.Upstream.(EmbeddingsProvider)
	if !ok {
		writeOpenAIError(w, http.StatusBadRequest, "unsupported_operation", fmt.Sprintf("channel %s does not support embeddings", kind))
		return
	}

	embeddings, promptTokens, err := embedder.EmbedTexts(acct, upstreamModel, inputs, req.Dimensions)
	if err != nil {
		status := http.StatusBadGateway
		var pe *provider.Error
		if isProviderError(err, &pe) {
			status = pe.Status
		}
		writeOpenAIError(w, status, "upstream_error", fmt.Sprintf("embeddings failed: %v", err))
		return
	}

	dataList := make([]embeddingData, len(embeddings))
	for i, vec := range embeddings {
		dataList[i] = embeddingData{
			Object:    "embedding",
			Embedding: vec,
			Index:     i,
		}
	}

	resp := embeddingResponse{
		Object: "list",
		Data:   dataList,
		Model:  req.Model,
		Usage: embeddingUsage{
			PromptTokens: promptTokens,
			TotalTokens:  promptTokens,
		},
	}

	writeJSON(w, http.StatusOK, resp)
}

type EmbeddingsProvider interface {
	EmbedTexts(a *auth.Auth, model string, texts []string, dimensions int) (embeddings [][]float64, promptTokens int, err error)
}

func parseEmbeddingInputs(raw any) ([]string, error) {
	if raw == nil {
		return nil, fmt.Errorf("missing required field: input")
	}
	switch v := raw.(type) {
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return nil, fmt.Errorf("input string cannot be empty")
		}
		return []string{v}, nil
	case []any:
		out := make([]string, 0, len(v))
		for i, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("input[%d] must be a string", i)
			}
			out = append(out, s)
		}
		return out, nil
	case []string:
		return v, nil
	default:
		return nil, fmt.Errorf("unsupported input type: must be string or array of strings")
	}
}

func isProviderError(err error, target **provider.Error) bool {
	if pe, ok := err.(*provider.Error); ok {
		*target = pe
		return true
	}
	return false
}
