package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUsagesRequiresKeyAndReturnsWindows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	_ = os.WriteFile(path, []byte(`{"providers":{"openai-codex":{"tokens":{"access_token":"test.access.token","refresh_token":"x"}}}}`), 0600)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/wham/usage" {
			t.Errorf("usage path=%s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test.access.token" {
			t.Errorf("missing upstream token")
		}
		_, _ = w.Write([]byte(`{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":2,"limit_window_seconds":18000,"reset_at":1792000000},"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":1792500000}},"rate_limit_reset_credits":{"available_count":3}}`))
	}))
	defer upstream.Close()
	g := &gateway{keys: []string{"local-key-0123456789"}, authPath: path, upstream: upstream.URL + "/codex", client: &http.Client{Timeout: time.Second}}
	unauthorized := httptest.NewRecorder()
	g.handler().ServeHTTP(unauthorized, httptest.NewRequest("GET", "/usages", nil))
	if unauthorized.Code != 401 {
		t.Fatal(unauthorized.Code)
	}
	req := httptest.NewRequest("GET", "/usages", nil)
	req.Header.Set("Authorization", "Bearer local-key-0123456789")
	rec := httptest.NewRecorder()
	g.handler().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"remaining_percent":98`) || !strings.Contains(rec.Body.String(), `"banked_resets":3`) {
		t.Fatalf("usage %d %s", rec.Code, rec.Body.String())
	}
}
