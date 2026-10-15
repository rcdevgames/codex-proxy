package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func apiError(w http.ResponseWriter, status int, kind, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"type": kind, "message": msg}})
}
func (g *gateway) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /usages", g.usage)
	mux.HandleFunc("GET /v1/usages", g.usage)
	mux.HandleFunc("GET /v1/models", g.listModels)
	mux.HandleFunc("POST /v1/responses", g.responses)
	mux.HandleFunc("POST /v1/chat/completions", g.chat)
	mux.HandleFunc("POST /v1/images/generations", g.images)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.authorized(r) {
			apiError(w, 401, "authentication_error", "invalid API key")
			return
		}
		mux.ServeHTTP(w, r)
	})
}
func (g *gateway) send(ctx context.Context, body map[string]any) (*http.Response, error) {
	body["stream"] = true
	body["store"] = false
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	token, err := g.tokenFor(ctx, false, "request")
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "POST", g.upstreamURL()+"/responses", bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("User-Agent", "codex-bridge/0.1")
		req.Header.Set("originator", "codex-bridge")
		accountHeaders(req.Header, token)
		resp, err := g.httpClient().Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != 401 || attempt == 1 {
			return resp, nil
		}
		resp.Body.Close()
		log.Printf("[refresh] upstream returned 401 on attempt %d; forcing token refresh", attempt+1)
		token, err = g.tokenFor(ctx, true, "upstream_401")
		if err != nil {
			return nil, err
		}
	}
	return nil, errors.New("retry exhausted")
}
func upstreamError(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	// The upstream body may carry credentials or private details, so only the
	// reason field is surfaced; the rest stays in the log as a byte count.
	log.Printf("upstream HTTP %d (%d bytes of body suppressed)", resp.StatusCode, len(b))
	msg := fmt.Sprintf("Codex upstream HTTP %d", resp.StatusCode)
	if reason := safeUpstreamMessage(b); reason != "" {
		msg += ": " + reason
	}
	apiError(w, resp.StatusCode, "upstream_error", msg)
}
func decodeBody(w http.ResponseWriter, r *http.Request) map[string]any {
	var body map[string]any
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&body); err != nil || body == nil {
		apiError(w, 400, "invalid_request_error", "invalid JSON body")
		return nil
	}
	if text(body["model"]) == "" {
		apiError(w, 400, "invalid_request_error", "model required")
		return nil
	}
	return body
}
func (g *gateway) responses(w http.ResponseWriter, r *http.Request) {
	body := decodeBody(w, r)
	if body == nil {
		return
	}
	resp, err := g.send(r.Context(), body)
	if err != nil {
		apiError(w, 502, "upstream_error", err.Error())
		return
	}
	if resp.StatusCode != 200 {
		upstreamError(w, resp)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	_, _ = io.Copy(w, resp.Body)
}
func messageText(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case []any:
		var b strings.Builder
		for _, item := range x {
			m := object(item)
			if text(m["type"]) != "text" {
				return "", errors.New("only text message parts supported")
			}
			b.WriteString(text(m["text"]))
		}
		return b.String(), nil
	default:
		return "", errors.New("only text messages supported")
	}
}
func chatInput(v any) ([]any, error) {
	messages, ok := v.([]any)
	if !ok || len(messages) == 0 {
		return nil, errors.New("messages must be a nonempty array")
	}
	input := make([]any, 0, len(messages))
	for _, item := range messages {
		m := object(item)
		role := text(m["role"])
		if role != "user" && role != "assistant" && role != "system" && role != "developer" {
			return nil, fmt.Errorf("unsupported role %q", role)
		}
		content, err := messageText(m["content"])
		if err != nil {
			return nil, err
		}
		if role == "system" {
			role = "developer"
		}
		typ := "input_text"
		if role == "assistant" {
			typ = "output_text"
		}
		input = append(input, map[string]any{"role": role, "content": []any{map[string]any{"type": typ, "text": content}}})
	}
	return input, nil
}
func (g *gateway) chat(w http.ResponseWriter, r *http.Request) {
	body := decodeBody(w, r)
	if body == nil {
		return
	}
	if body["tools"] != nil || body["tool_choice"] != nil || body["response_format"] != nil {
		apiError(w, 400, "unsupported_feature", "tools and structured output are not supported")
		return
	}
	input, err := chatInput(body["messages"])
	if err != nil {
		apiError(w, 400, "invalid_request_error", err.Error())
		return
	}
	model := text(body["model"])
	request := map[string]any{"model": model, "input": input, "stream": true, "store": false}
	if v, ok := body["max_completion_tokens"]; ok {
		request["max_output_tokens"] = v
	}
	if v, ok := body["reasoning_effort"]; ok {
		request["reasoning"] = map[string]any{"effort": v}
	}
	resp, err := g.send(r.Context(), request)
	if err != nil {
		apiError(w, 502, "upstream_error", err.Error())
		return
	}
	if resp.StatusCode != 200 {
		upstreamError(w, resp)
		return
	}
	defer resp.Body.Close()
	stream, _ := body["stream"].(bool)
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 2<<20)
	var output strings.Builder
	usage := map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	id := "chatcmpl-" + randomID()
	sendChunk := func(delta map[string]any, finish any) {
		writeSSE(w, map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
	}
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil {
			continue
		}
		switch text(event["type"]) {
		case "response.output_text.delta":
			delta := text(event["delta"])
			output.WriteString(delta)
			if stream {
				sendChunk(map[string]any{"content": delta}, nil)
			}
		case "response.completed":
			u := object(object(event["response"])["usage"])
			in, out := number(u["input_tokens"]), number(u["output_tokens"])
			usage = map[string]any{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out}
		case "response.failed":
			if stream {
				writeSSE(w, map[string]any{"error": map[string]any{"message": "upstream response failed"}})
				return
			}
			apiError(w, 502, "upstream_error", "upstream response failed")
			return
		}
	}
	if err := scanner.Err(); err != nil {
		if !stream {
			apiError(w, 502, "upstream_error", err.Error())
		}
		return
	}
	if stream {
		sendChunk(map[string]any{}, "stop")
		fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": output.String()}, "finish_reason": "stop"}}, "usage": usage})
}
func writeSSE(w http.ResponseWriter, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(w, "data: %s\n\n", b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
func randomID() string { var b [12]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }
func main() {
	cfgPath := flag.String("config", "config.yaml", "gateway YAML config file")
	flag.Parse()
	binaryPath, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	cfg, authPath, err := loadConfig(*cfgPath, binaryPath)
	if err != nil {
		log.Fatal(err)
	}
	g := &gateway{keys: cfg.APIKeys, authPath: authPath, client: &http.Client{Timeout: 3 * time.Minute}}
	log.Printf("auth source: %s", authPath)
	g.logCredentialState()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.scheduler(ctx, time.Duration(cfg.RefreshCheckSeconds)*time.Second)
	log.Printf("listening on %s", cfg.address())
	srv := &http.Server{Addr: cfg.address(), Handler: g.handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
