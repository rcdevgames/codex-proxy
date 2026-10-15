package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
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
)

// validateImageRequest returns a client-facing message, or "" when the body is
// acceptable. It never echoes the prompt back.
func validateImageRequest(body map[string]any) string {
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
	if raw, present := body["n"]; present {
		n, ok := raw.(float64)
		if !ok || n != float64(int(n)) || n < 1 || n > maxImageN {
			return "n must be 1 (only one image per request is allowed)"
		}
	}
	return ""
}

func (g *gateway) images(w http.ResponseWriter, r *http.Request) {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxImageBody))
	var body map[string]any
	if err := dec.Decode(&body); err != nil || body == nil {
		apiError(w, 400, "invalid_request_error", "invalid JSON body")
		return
	}
	if msg := validateImageRequest(body); msg != "" {
		apiError(w, 400, "invalid_request_error", msg)
		return
	}
	// Stream/store flags belong to the Responses API only; never forwarded here.
	delete(body, "stream")
	delete(body, "store")
	payload, err := json.Marshal(body)
	if err != nil {
		apiError(w, 400, "invalid_request_error", "invalid request")
		return
	}
	resp, err := g.postImage(r.Context(), payload)
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
	log.Printf("[images] ok status=200")
}

// postImage mirrors send(): one token refresh on upstream 401, then give up.
func (g *gateway) postImage(ctx context.Context, payload []byte) (*http.Response, error) {
	token, err := g.tokenFor(ctx, false, "request")
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "POST", g.upstreamURL()+"/images/generations", bytes.NewReader(payload))
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
