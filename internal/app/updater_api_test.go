package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"wild-work/internal/updater"
)

func TestUpdaterAPIStatusAndDismiss(t *testing.T) {
	a := newPanelApp(t, "127.0.0.1", "")
	mux := panelMux(a)

	// 1. GET /api/update/status
	w := doReq(mux, "GET", "/api/update/status", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/update/status code = %d, body = %s", w.Code, w.Body.String())
	}
	var st updater.State
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("json parse error: %v", err)
	}
	if st.CurrentVersion != Version {
		t.Errorf("expected current version %q, got %q", Version, st.CurrentVersion)
	}

	// 2. POST /api/update/dismiss
	w = doReq(mux, "POST", "/api/update/dismiss", `{"version":"v99.9.9"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/update/dismiss code = %d, body = %s", w.Code, w.Body.String())
	}

	// 3. GetState() includes Update
	appState := a.GetState()
	if appState.Update == nil {
		t.Fatalf("appState.Update should not be nil")
	}
	if appState.Update.CurrentVersion != Version {
		t.Errorf("expected CurrentVersion %q, got %q", Version, appState.Update.CurrentVersion)
	}
}

func TestUpdaterAPICheckWithMockServer(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v9.9.9",
			"body":     "Release v9.9.9 notes",
			"assets": []map[string]any{
				{
					"name":                 "wild-work-darwin-arm64",
					"browser_download_url": "http://127.0.0.1/download/darwin-arm64",
					"size":                 1024,
					"digest":               "sha256:1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
				},
			},
		})
	}))
	defer ts.Close()

	a := newPanelApp(t, "127.0.0.1", "")
	// Configure updater service with test mock
	upCfg := updater.Config{
		CurrentVersion: "v1.0.0",
		Repo:           "test/repo",
		Proxies:        []string{ts.URL + "/"},
	}
	a.updater = updater.NewService(upCfg, ts.Client(), ts.Client())
	mux := panelMux(a)

	w := doReq(mux, "POST", "/api/update/check", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/update/check code = %d, body = %s", w.Code, w.Body.String())
	}
	var st updater.State
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if st.Release == nil || st.Release.TagName != "v9.9.9" {
		t.Errorf("expected latest release v9.9.9, got %+v", st.Release)
	}
	if st.Status != updater.StatusAvailable {
		t.Errorf("expected StatusAvailable, got %v", st.Status)
	}
}
