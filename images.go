package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Only the rules below are enforced locally. Each one matches a rejection the
// upstream actually returned (empty or missing prompt). Anything else is left
// to upstream so valid requests are never blocked here.
const (
	maxImagePrompt = 4000 // local cap: upstream limit unknown, keep prompts sane
	maxImageN      = 1    // local cost guard: one image per request by default
	maxImageBody   = 1 << 20
	maxEditBody    = 32 << 20 // base64 payloads grow ~4/3; upstream caps each image at 50MB
	maxEditImages  = 16       // upstream limit for GPT Image edit inputs
	maxEditImageMB = 50
)

// validateImageRequest returns a client-facing message, or "" when the body is
// acceptable. It never echoes the prompt back.
func validateImageRequest(body map[string]any) string {
	if msg := validateImagePrompt(body); msg != "" {
		return msg
	}
	return validateImageN(body)
}

// validateImageEditRequest is the same contract for /images/edits, plus the
// input images list the upstream requires (JSON shape: [{"image_url": dataURL}]).
func validateImageEditRequest(body map[string]any) string {
	if msg := validateImagePrompt(body); msg != "" {
		return msg
	}
	if msg := validateImageN(body); msg != "" {
		return msg
	}
	raw, present := body["images"]
	if !present {
		return "images is required"
	}
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return "images must be a nonempty array"
	}
	if len(list) > maxEditImages {
		return fmt.Sprintf("images must contain at most %d items", maxEditImages)
	}
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return fmt.Sprintf("images[%d] must be an object with image_url", i)
		}
		u, ok := m["image_url"].(string)
		if !ok || !strings.HasPrefix(u, "data:image/") {
			return fmt.Sprintf("images[%d].image_url must be a data:image/... URL", i)
		}
	}
	return ""
}

func validateImagePrompt(body map[string]any) string {
	prompt, ok := body["prompt"].(string)
	if !ok {
		return "prompt is required and must be a string"
	}
	if strings.TrimSpace(prompt) == "" {
		return "prompt must not be empty"
	}
	if len([]rune(prompt)) > maxImagePrompt {
		return "prompt exceeds 4000 characters"
	}
	return ""
}

func validateImageN(body map[string]any) string {
	if raw, present := body["n"]; present {
		n, ok := raw.(float64)
		if !ok || n != float64(int(n)) || n < 1 || n > maxImageN {
			return fmt.Sprintf("n must be %d (only one image per request is allowed)", maxImageN)
		}
	}
	return ""
}

func (g *gateway) images(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeImageJSON(w, r, maxImageBody)
	if !ok {
		return
	}
	if msg := validateImageRequest(body); msg != "" {
		apiError(w, 400, "invalid_request_error", msg)
		return
	}
	g.proxyImage(w, r, "/images/generations", body)
}

func (g *gateway) imageEdits(w http.ResponseWriter, r *http.Request) {
	body, ok := g.decodeImageEdit(w, r)
	if !ok {
		return
	}
	if msg := validateImageEditRequest(body); msg != "" {
		apiError(w, 400, "invalid_request_error", msg)
		return
	}
	g.proxyImage(w, r, "/images/edits", body)
}

// decodeImageEdit accepts both shapes: native JSON (what the upstream wants)
// and multipart/form-data (what OpenAI SDKs send). Multipart is converted here
// so callers can post an image file without base64-encoding it themselves.
func (g *gateway) decodeImageEdit(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		return decodeImageEditMultipart(w, r)
	}
	return decodeImageJSON(w, r, maxEditBody)
}

func decodeImageJSON(w http.ResponseWriter, r *http.Request, limit int64) (map[string]any, bool) {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	var body map[string]any
	if err := dec.Decode(&body); err != nil || body == nil {
		apiError(w, 400, "invalid_request_error", "invalid JSON body")
		return nil, false
	}
	return body, true
}

func decodeImageEditMultipart(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxEditBody)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		apiError(w, 400, "invalid_request_error", "invalid multipart body")
		return nil, false
	}
	body := map[string]any{}
	for _, field := range []string{"model", "prompt", "size", "quality", "background", "output_format", "moderation"} {
		if v := r.FormValue(field); v != "" {
			body[field] = v
		}
	}
	for _, field := range []string{"n", "output_compression"} {
		raw := r.FormValue(field)
		if raw == "" {
			continue
		}
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			apiError(w, 400, "invalid_request_error", field+" must be a non-negative integer")
			return nil, false
		}
		body[field] = float64(v)
	}
	var images []any
	if r.MultipartForm != nil {
		for field, files := range r.MultipartForm.File {
			switch field {
			case "image", "image[]", "images", "images[]":
				for _, fh := range files {
					u, err := imageFileDataURL(fh)
					if err != nil {
						apiError(w, 400, "invalid_request_error", err.Error())
						return nil, false
					}
					images = append(images, map[string]any{"image_url": u})
				}
			case "mask":
				for _, fh := range files {
					u, err := imageFileDataURL(fh)
					if err != nil {
						apiError(w, 400, "invalid_request_error", err.Error())
						return nil, false
					}
					body["mask"] = map[string]any{"image_url": u}
				}
			}
		}
	}
	if len(images) > 0 {
		body["images"] = images
	}
	return body, true
}

// imageFileDataURL reads one uploaded image into the data URL the upstream
// expects. The upstream limit per input image is 50MB.
func imageFileDataURL(fh *multipart.FileHeader) (string, error) {
	if fh.Size > maxEditImageMB<<20 {
		return "", fmt.Errorf("%s exceeds %dMB", fh.Filename, maxEditImageMB)
	}
	f, err := fh.Open()
	if err != nil {
		return "", errors.New("cannot read uploaded image")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (maxEditImageMB<<20)+1))
	if err != nil {
		return "", errors.New("cannot read uploaded image")
	}
	if len(data) > maxEditImageMB<<20 {
		return "", fmt.Errorf("%s exceeds %dMB", fh.Filename, maxEditImageMB)
	}
	contentType := fh.Header.Get("Content-Type")
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = http.DetectContentType(data)
	}
	if !strings.HasPrefix(contentType, "image/") {
		return "", fmt.Errorf("%s is not an image", fh.Filename)
	}
	return "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// proxyImage forwards a validated body upstream and streams the reply back.
// Stream/store flags belong to the Responses API only; never forwarded here.
func (g *gateway) proxyImage(w http.ResponseWriter, r *http.Request, endpoint string, body map[string]any) {
	delete(body, "stream")
	delete(body, "store")
	payload, err := json.Marshal(body)
	if err != nil {
		apiError(w, 400, "invalid_request_error", "invalid request")
		return
	}
	resp, err := g.postImage(r.Context(), endpoint, payload)
	if err != nil {
		apiError(w, 502, "upstream_error", "image upstream unavailable")
		log.Printf("[images] upstream error: %v", err)
		return
	}
	if resp.StatusCode != 200 {
		upstreamError(w, resp)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 64<<20))
	log.Printf("[images] ok endpoint=%s status=200", endpoint)
}

// postImage mirrors send(): one token refresh on upstream 401, then give up.
func (g *gateway) postImage(ctx context.Context, endpoint string, payload []byte) (*http.Response, error) {
	token, err := g.tokenFor(ctx, false, "request")
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "POST", g.upstreamURL()+endpoint, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "codex-bridge/0.1")
		req.Header.Set("originator", "codex-bridge")
		accountHeaders(req.Header, token)
		resp, err := g.imageClient().Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != 401 || attempt == 1 {
			return resp, nil
		}
		resp.Body.Close()
		log.Printf("[refresh] image upstream returned 401 on attempt %d; forcing token refresh", attempt+1)
		token, err = g.tokenFor(ctx, true, "upstream_401")
		if err != nil {
			return nil, err
		}
	}
	return nil, errors.New("retry exhausted")
}

// Image generation can take well over the chat timeout; allow it explicitly.
func (g *gateway) imageClient() *http.Client {
	if g.client != nil {
		return g.client
	}
	return &http.Client{Timeout: 5 * time.Minute}
}
