package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConcurrentRefreshOnlyRotatesOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	old := jwtWithExpiry(time.Now().Add(-time.Minute))
	fresh := jwtWithExpiry(time.Now().Add(time.Hour))
	_ = os.WriteFile(path, []byte(fmt.Sprintf(`{"providers":{"openai-codex":{"tokens":{"access_token":%q,"refresh_token":"old"}}}}`, old)), 0600)
	calls := 0
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		time.Sleep(40 * time.Millisecond)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"access_token":%q,"refresh_token":"new"}`, fresh)))
	}))
	defer server.Close()
	g := &gateway{authPath: path, tokenURL: server.URL, client: &http.Client{Timeout: time.Second}}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := g.token(context.Background(), false)
			if err != nil || token != fresh {
				t.Errorf("token error %v", err)
			}
		}()
	}
	wg.Wait()
	if calls != 1 {
		t.Fatalf("refresh calls=%d", calls)
	}
}
func jwtWithExpiry(exp time.Time) string {
	b, _ := json.Marshal(map[string]any{"exp": exp.Unix()})
	return "x." + base64.RawURLEncoding.EncodeToString(b) + ".x"
}
func TestResponsesPassesThroughSSEAndRequiresKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	_ = os.WriteFile(path, []byte(`{"providers":{"openai-codex":{"tokens":{"access_token":"test.access.token","refresh_token":"x"}}}}`), 0600)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("event: response.output_text.delta\ndata: {\"delta\":\"ok\"}\n\n"))
	}))
	defer upstream.Close()
	g := &gateway{keys: []string{"local-key-0123456789"}, authPath: path, upstream: upstream.URL}
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-5-codex","input":"ping","stream":true}`))
	req.Header.Set("Authorization", "Bearer local-key-0123456789")
	rec := httptest.NewRecorder()
	g.handler().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "event: response.output_text.delta") {
		t.Fatalf("response %d %s", rec.Code, rec.Body.String())
	}
}
