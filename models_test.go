package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func modelsGateway(t *testing.T, upstream *httptest.Server) *gateway {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	store := `{"providers":{"openai-codex":{"tokens":{"access_token":"test.access.token","refresh_token":"secret"}}}}`
	if err := os.WriteFile(path, []byte(store), 0600); err != nil {
		t.Fatal(err)
	}
	return &gateway{keys: []string{"local-key-0123456789"}, authPath: path, upstream: upstream.URL, client: &http.Client{Timeout: time.Minute}}
}

func TestModelsListsVisibleUpstreamSlugs(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/models" {
			t.Errorf("path=%s", r.URL.Path)
		}
		if r.URL.Query().Get("client_version") == "" {
			t.Error("client_version missing")
		}
		if r.Header.Get("Authorization") != "Bearer test.access.token" {
			t.Errorf("upstream auth=%q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[
			{"slug":"gpt-6-astra","visibility":"list"},
			{"slug":"gpt-6-sol","visibility":"list"},
			{"slug":"gpt-reserve","visibility":"hide"}
		]}`))
	}))
	defer upstream.Close()
	g := modelsGateway(t, upstream)

	call := func() (int, []string) {
		req := httptest.NewRequest("GET", "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer local-key-0123456789")
		rec := httptest.NewRecorder()
		g.handler().ServeHTTP(rec, req)
		var body struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %s: %v", rec.Body.String(), err)
		}
		ids := make([]string, 0, len(body.Data))
		for _, m := range body.Data {
			ids = append(ids, m.ID)
		}
		return rec.Code, ids
	}

	code, ids := call()
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if strings.Join(ids, ",") != "gpt-6-astra,gpt-6-sol" {
		t.Fatalf("ids=%v (hidden slug must not be advertised)", ids)
	}
	if _, ids = call(); strings.Join(ids, ",") != "gpt-6-astra,gpt-6-sol" {
		t.Fatalf("second call ids=%v", ids)
	}
	if calls != 1 {
		t.Fatalf("upstream model list fetched %d times, want 1 (cached)", calls)
	}
}

func TestModelsFallsBackWhenUpstreamFails(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
	}))
	defer upstream.Close()
	g := modelsGateway(t, upstream)

	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer local-key-0123456789")
	rec := httptest.NewRecorder()
	g.handler().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), defaultModel) {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
}

func TestModelsRequiresGatewayKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be reached without a gateway key")
	}))
	defer upstream.Close()
	g := modelsGateway(t, upstream)

	rec := httptest.NewRecorder()
	g.handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 401 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
}

func TestUpstreamErrorSurfacesReasonWithoutLeakingBody(t *testing.T) {
	token := "eyJhbGciOiJSUzI1NiJ9." + strings.Repeat("A", 120) + ".signature"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"The 'gpt-5-codex' model is not supported when using Codex with a ChatGPT account.","refresh_token":"` + token + `"}`))
	}))
	defer upstream.Close()
	g := modelsGateway(t, upstream)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5-codex","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer local-key-0123456789")
	rec := httptest.NewRecorder()
	g.handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if rec.Code != 400 {
		t.Fatalf("status %d body %s", rec.Code, body)
	}
	if !strings.Contains(body, "not supported when using Codex") {
		t.Fatalf("reason not surfaced: %s", body)
	}
	if strings.Contains(body, token) || strings.Contains(body, "refresh_token") {
		t.Fatalf("upstream body leaked: %s", body)
	}
}

func TestSafeUpstreamMessage(t *testing.T) {
	long := strings.Repeat("word ", 100)
	for _, tc := range []struct{ name, body, want string }{
		{"detail wins", `{"detail":"model not supported"}`, "model not supported"},
		{"error message", `{"error":{"message":"bad request"}}`, "bad request"},
		{"whitespace collapsed", "{\"detail\":\"a\\n  b\"}", "a b"},
		{"non-json dropped", `not json at all`, ""},
		{"truncated", `{"detail":"` + long + `"}`, strings.TrimSpace(long)[:300] + "..."},
	} {
		if got := safeUpstreamMessage([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
	secret := strings.Repeat("Z", 100)
	if got := safeUpstreamMessage([]byte(`{"detail":"key ` + secret + ` rejected"}`)); strings.Contains(got, secret) {
		t.Errorf("credential run not redacted: %q", got)
	}
}
