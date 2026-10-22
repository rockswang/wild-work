package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/gemini"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

func TestEmbeddingsEndpoint(t *testing.T) {
	p := pool.New(t.TempDir() + "/state-gemini.json")
	p.Add(&auth.Auth{UID: "gemini-uid", AccessToken: "AT-ready", ExpiresAt: time.Now().Add(24 * time.Hour).Unix()})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1internal:embedContent" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"embedding": {"values": [0.1, 0.2, 0.3, 0.4]}}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	client := gemini.NewWithBase(srv.URL)

	h := NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.Gemini: {
				Kind:     provider.Gemini,
				Pool:     p,
				Upstream: client,
			},
		},
	})

	reqBody := []byte(`{"model": "gemini/text-embedding-004", "input": ["hello world", "test 2"]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d, body: %s", w.Code, w.Body.String())
	}

	var resp embeddingResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("expected 2 embedding items, got %d", len(resp.Data))
	}
	if resp.Model != "gemini/text-embedding-004" {
		t.Fatalf("unexpected model: %s", resp.Model)
	}
}

func TestImagesGenerationsGeminiImagen(t *testing.T) {
	p := pool.New(t.TempDir() + "/state-gemini-img.json")
	p.Add(&auth.Auth{UID: "gemini-uid", AccessToken: "AT-ready", ExpiresAt: time.Now().Add(24 * time.Hour).Unix()})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1internal:predict" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"predictions": [{"bytesBase64Encoded": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==", "mimeType": "image/png"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	client := gemini.NewWithBase(srv.URL)

	h := NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.Gemini: {
				Kind:     provider.Gemini,
				Pool:     p,
				Upstream: client,
			},
		},
	})

	reqBody := []byte(`{"model": "gemini/imagen-3.0-generate-002", "prompt": "a futuristic city", "n": 1}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d, body: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("expected 1 image data item, got %d", len(resp.Data))
	}
	if resp.Data[0]["b64_json"] == "" {
		t.Fatalf("expected b64_json in image data, got %v", resp.Data[0])
	}
}