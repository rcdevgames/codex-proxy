package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	// codexClientVersion is sent as the client_version query parameter the
	// upstream model list requires; the endpoint rejects the request without it.
	codexClientVersion = "0.155.0"
	// defaultModel is the fallback model id when the upstream list is unavailable.
	defaultModel = "gpt-6-sol"
	// modelsCacheTTL keeps the upstream model list from being fetched per request.
	modelsCacheTTL = 10 * time.Minute
)

// listModels serves the model ids the upstream account actually exposes.
// The upstream list is authoritative: a ChatGPT-plan Codex account accepts only
// the slugs it reports, so a hardcoded list would advertise models that fail.
func (g *gateway) listModels(w http.ResponseWriter, r *http.Request) {
	ids, err := g.modelIDs(r.Context())
	if err != nil {
		log.Printf("model list unavailable, falling back to %s: %v", defaultModel, err)
		ids = []string{defaultModel}
	}
	data := make([]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{"id": id, "object": "model"})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

func (g *gateway) modelIDs(ctx context.Context) ([]string, error) {
	g.modelsMu.Lock()
	if len(g.models) > 0 && time.Since(g.modelsAt) < modelsCacheTTL {
		ids := append([]string(nil), g.models...)
		g.modelsMu.Unlock()
		return ids, nil
	}
	g.modelsMu.Unlock()

	// token() takes the auth mutex; never call it while holding modelsMu.
	token, err := g.token(ctx, false)
	if err != nil {
		return nil, err
	}
	url := g.upstreamURL() + "/models?client_version=" + codexClientVersion
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex-bridge/0.1")
	req.Header.Set("originator", "codex-bridge")
	accountHeaders(req.Header, token)
	resp, err := g.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("upstream model list HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Models []struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("model list response: %w", err)
	}
	ids := make([]string, 0, len(payload.Models))
	for _, m := range payload.Models {
		// Hidden entries are internal to the CLI; do not advertise them.
		if m.Slug == "" || m.Visibility == "hide" {
			continue
		}
		ids = append(ids, m.Slug)
	}
	if len(ids) == 0 {
		return nil, errors.New("upstream model list is empty")
	}
	g.modelsMu.Lock()
	g.models, g.modelsAt = ids, time.Now()
	g.modelsMu.Unlock()
	return append([]string(nil), ids...), nil
}

// tokenRun matches credential-shaped strings (JWTs, API keys) so they can be
// stripped from any message that is echoed back to a client.
var tokenRun = regexp.MustCompile(`[A-Za-z0-9_\-\.]{80,}`)

// safeUpstreamMessage extracts the human-readable reason from an upstream error
// body without echoing the body wholesale: only detail / error.message is used,
// truncated, and credential-shaped runs are redacted.
func safeUpstreamMessage(body []byte) string {
	var payload struct {
		Detail string `json:"detail"`
		Error  struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := ""
	if json.Unmarshal(body, &payload) == nil {
		msg = strings.TrimSpace(payload.Detail)
		if msg == "" {
			msg = strings.TrimSpace(payload.Error.Message)
		}
	}
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	return tokenRun.ReplaceAllString(msg, "[REDACTED]")
}
