package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGatewayAuthAndChat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"providers":{"openai-codex":{"tokens":{"access_token":"test.access.token","refresh_token":"secret"},"last_refresh":"yesterday","auth_mode":"chatgpt"}},"other":{"keep":true}}`), 0600); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("path=%s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test.access.token" {
			t.Errorf("upstream auth")
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req["stream"] != true || req["store"] != false {
			t.Errorf("request=%v", req)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n"))
	}))
	defer upstream.Close()
	g := &gateway{keys: []string{"local-key-0123456789"}, authPath: path, upstream: upstream.URL, client: &http.Client{Timeout: time.Minute}}
	for _, tc := range []struct {
		key    string
		status int
	}{{"", 401}, {"wrong", 401}, {"local-key-0123456789", 200}} {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5-codex","messages":[{"role":"user","content":"ping"}]}`))
		req.Header.Set("Authorization", "Bearer "+tc.key)
		rec := httptest.NewRecorder()
		g.handler().ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
		if tc.status == 200 && !strings.Contains(rec.Body.String(), "pong") {
			t.Fatalf("response %s", rec.Body.String())
		}
	}
}

func TestRefreshPreservesStoreAndRotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	_ = os.WriteFile(path, []byte(`{"version":1,"providers":{"openai-codex":{"tokens":{"access_token":"old.access.token","refresh_token":"old-refresh"}}},"credential_pool":{"openai-codex":[{"source":"device_code","access_token":"old.access.token","refresh_token":"old-refresh"}]},"other":{"keep":true}}`), 0600)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("refresh_token") != "old-refresh" {
			t.Errorf("wrong refresh")
		}
		_, _ = w.Write([]byte(`{"access_token":"new.access.token","refresh_token":"new-refresh"}`))
	}))
	defer srv.Close()
	g := &gateway{authPath: path, tokenURL: srv.URL, client: &http.Client{Timeout: time.Second}}
	if _, err := g.token(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	var v map[string]any
	_ = json.Unmarshal(b, &v)
	if v["other"].(map[string]any)["keep"] != true {
		t.Fatalf("lost unrelated data")
	}
	p := v["providers"].(map[string]any)["openai-codex"].(map[string]any)["tokens"].(map[string]any)
	if p["refresh_token"] != "new-refresh" {
		t.Fatal("rotation not saved")
	}
	pool := v["credential_pool"].(map[string]any)["openai-codex"].([]any)[0].(map[string]any)
	if pool["refresh_token"] != "new-refresh" {
		t.Fatal("pool not synchronized")
	}
}
