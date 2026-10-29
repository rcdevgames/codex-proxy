package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
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

// editGateway is imageGateway plus the upstream path, since /images/edits and
// /images/generations share one handler shape but must not be confused.
func editGateway(t *testing.T, status int, reply string) (g *gateway, calls *int, path *string, seen *[]byte) {
	t.Helper()
	access := jwtWithExpiry(time.Now().Add(time.Hour))
	authPath := filepath.Join(t.TempDir(), "auth.json")
	store := `{"providers":{"openai-codex":{"tokens":{"access_token":"` + access + `","refresh_token":"r"}}}}`
	if err := os.WriteFile(authPath, []byte(store), 0600); err != nil {
		t.Fatal(err)
	}
	calls = new(int)
	path = new(string)
	seen = new([]byte)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		*path = r.URL.Path
		*seen = make([]byte, r.ContentLength)
		_, _ = r.Body.Read(*seen)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(up.Close)
	g = &gateway{keys: []string{"k"}, authPath: authPath, upstream: up.URL, client: &http.Client{Timeout: 5 * time.Second}}
	return g, calls, path, seen
}

func postImageEdit(t *testing.T, g *gateway, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/images/edits", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	g.handler().ServeHTTP(rec, req)
	return rec
}

const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func TestImageEditsRejectsInvalidInputLocally(t *testing.T) {
	img := `{"image_url":"data:image/png;base64,` + onePixelPNG + `"}`
	cases := map[string]string{
		"missing prompt":    `{"images":[` + img + `]}`,
		"empty prompt":      `{"prompt":"  ","images":[` + img + `]}`,
		"missing images":    `{"prompt":"x"}`,
		"images not array":  `{"prompt":"x","images":"` + img + `"}`,
		"images empty":      `{"prompt":"x","images":[]}`,
		"image not object":  `{"prompt":"x","images":["data:image/png;base64,AAA"]}`,
		"missing image_url": `{"prompt":"x","images":[{}]}`,
		"not a data URL":    `{"prompt":"x","images":[{"image_url":"https://example.com/a.png"}]}`,
		"n two":             `{"prompt":"x","n":2,"images":[` + img + `]}`,
		"prompt too long":   `{"prompt":"` + strings.Repeat("a", 4001) + `","images":[` + img + `]}`,
		"not json":          `{"prompt":`,
	}
	for name, body := range cases {
		g, calls, _, _ := editGateway(t, 200, `{"data":[]}`)
		rec := postImageEdit(t, g, "application/json", body)
		if rec.Code != 400 {
			t.Errorf("%s: status=%d want 400 body=%s", name, rec.Code, rec.Body)
		}
		if *calls != 0 {
			t.Errorf("%s: invalid input reached upstream (%d calls)", name, *calls)
		}
	}
}

func TestImageEditsForwardsJSONToEditsEndpoint(t *testing.T) {
	g, calls, path, seen := editGateway(t, 200, `{"created":1,"data":[{"b64_json":"AAA"}]}`)
	body := `{"model":"gpt-image-1-mini","prompt":"make it blue","size":"1024x1024","images":[{"image_url":"data:image/png;base64,` + onePixelPNG + `"}],"stream":true,"store":true}`
	rec := postImageEdit(t, g, "application/json", body)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if *calls != 1 {
		t.Fatalf("upstream calls=%d want 1", *calls)
	}
	if *path != "/images/edits" {
		t.Errorf("upstream path=%q want /images/edits", *path)
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
	if sent["prompt"] != "make it blue" || sent["model"] != "gpt-image-1-mini" {
		t.Errorf("valid fields not forwarded: %v", sent)
	}
	if !strings.Contains(rec.Body.String(), `"b64_json":"AAA"`) {
		t.Errorf("upstream payload not returned: %s", rec.Body)
	}
}

func TestImageEditsAcceptsMultipartUpload(t *testing.T) {
	png, err := base64.StdEncoding.DecodeString(onePixelPNG)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range map[string]string{"prompt": "make the square blue", "model": "gpt-image-1-mini", "size": "1024x1024"} {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	head := textproto.MIMEHeader{}
	head.Set("Content-Disposition", `form-data; name="image"; filename="square.png"`)
	head.Set("Content-Type", "image/png")
	part, err := mw.CreatePart(head)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(png); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	g, calls, path, seen := editGateway(t, 200, `{"data":[{"b64_json":"AAA"}]}`)
	rec := postImageEdit(t, g, mw.FormDataContentType(), buf.String())
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if *calls != 1 || *path != "/images/edits" {
		t.Fatalf("calls=%d path=%q want 1 /images/edits", *calls, *path)
	}
	var sent map[string]any
	if err := json.Unmarshal(*seen, &sent); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	if sent["prompt"] != "make the square blue" {
		t.Errorf("prompt not forwarded: %v", sent["prompt"])
	}
	list, ok := sent["images"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("images=%v want one entry", sent["images"])
	}
	u, _ := list[0].(map[string]any)["image_url"].(string)
	if !strings.HasPrefix(u, "data:image/png;base64,") {
		t.Errorf("uploaded image not converted to data URL: %.40q", u)
	}
}

func TestImageEditsRejectsNonImageUpload(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("prompt", "x")
	part, _ := mw.CreateFormFile("image", "notes.txt")
	_, _ = part.Write([]byte("this is not an image"))
	_ = mw.Close()

	g, calls, _, _ := editGateway(t, 200, `{}`)
	rec := postImageEdit(t, g, mw.FormDataContentType(), buf.String())
	if rec.Code != 400 {
		t.Errorf("status=%d want 400 body=%s", rec.Code, rec.Body)
	}
	if *calls != 0 {
		t.Errorf("invalid upload reached upstream (%d calls)", *calls)
	}
}
