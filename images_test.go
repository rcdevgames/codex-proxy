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

// imageGateway wires a gateway to a fake upstream. The fake records every
// request it receives so tests can prove invalid input never reaches upstream.
func imageGateway(t *testing.T, status int, reply string) (*gateway, *int, *[]byte) {
	t.Helper()
	access := jwtWithExpiry(time.Now().Add(time.Hour))
	path := filepath.Join(t.TempDir(), "auth.json")
	store := `{"providers":{"openai-codex":{"tokens":{"access_token":"` + access + `","refresh_token":"r"}}}}`
	if err := os.WriteFile(path, []byte(store), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	var seen []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		seen = make([]byte, r.ContentLength)
		_, _ = r.Body.Read(seen)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(up.Close)
	g := &gateway{keys: []string{"k"}, authPath: path, upstream: up.URL, client: &http.Client{Timeout: 5 * time.Second}}
	return g, &calls, &seen
}

func postImages(t *testing.T, g *gateway, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	g.handler().ServeHTTP(rec, req)
	return rec
}

func TestImagesRejectsInvalidInputLocally(t *testing.T) {
	cases := map[string]string{
		"missing prompt":  `{"n":1}`,
		"empty prompt":    `{"prompt":"   "}`,
		"non-string":      `{"prompt":42}`,
		"n zero":          `{"prompt":"x","n":0}`,
		"n two":           `{"prompt":"x","n":2}`,
		"n fractional":    `{"prompt":"x","n":1.5}`,
		"prompt too long": `{"prompt":"` + strings.Repeat("a", 4001) + `"}`,
		"not json":        `{"prompt":`,
	}
	for name, body := range cases {
		g, calls, _ := imageGateway(t, 200, `{"data":[]}`)
		rec := postImages(t, g, body)
		if rec.Code != 400 {
			t.Errorf("%s: status=%d want 400 body=%s", name, rec.Code, rec.Body)
		}
		if *calls != 0 {
			t.Errorf("%s: invalid input reached upstream (%d calls)", name, *calls)
		}
	}
}

func TestImagesErrorDoesNotEchoPrompt(t *testing.T) {
	g, _, _ := imageGateway(t, 200, `{"data":[]}`)
	secret := "super-secret-prompt-text"
	rec := postImages(t, g, `{"prompt":"   ","n":1,"x":"`+secret+`"}`)
	if strings.Contains(rec.Body.String(), secret) {
		t.Errorf("error leaked request content: %s", rec.Body)
	}
}

func TestImagesForwardsValidRequestAndStripsResponsesFlags(t *testing.T) {
	g, calls, seen := imageGateway(t, 200, `{"created":1,"data":[{"b64_json":"AAA"}]}`)
	rec := postImages(t, g, `{"prompt":"a red dot","size":"1024x1024","stream":true,"store":true}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if *calls != 1 {
		t.Fatalf("upstream calls=%d want 1", *calls)
	}
	var sent map[string]any
	if err := json.Unmarshal(*seen, &sent); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	if _, ok := sent["stream"]; ok {
		t.Error("stream flag must not be forwarded to images endpoint")
	}
	if _, ok := sent["store"]; ok {
		t.Error("store flag must not be forwarded to images endpoint")
	}
	if sent["prompt"] != "a red dot" || sent["size"] != "1024x1024" {
		t.Errorf("valid fields not forwarded: %v", sent)
	}
	if !strings.Contains(rec.Body.String(), `"b64_json":"AAA"`) {
		t.Errorf("upstream payload not returned: %s", rec.Body)
	}
}

func TestImagesRequiresGatewayKey(t *testing.T) {
	g, calls, _ := imageGateway(t, 200, `{}`)
	req := httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(`{"prompt":"x"}`))
	rec := httptest.NewRecorder()
	g.handler().ServeHTTP(rec, req)
	if rec.Code != 401 || *calls != 0 {
		t.Errorf("status=%d calls=%d want 401/0", rec.Code, *calls)
	}
}
