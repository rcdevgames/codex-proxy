package main

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const codexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
const defaultTokenURL = "https://auth.openai.com/oauth/token"
const defaultUpstream = "https://chatgpt.com/backend-api/codex"

type gateway struct {
	keys                         []string
	authPath, upstream, tokenURL string
	client                       *http.Client
	mu                           sync.Mutex
	modelsMu                     sync.Mutex
	models                       []string
	modelsAt                     time.Time
	usageStats                   *usageTracker
	monitorSecret                string
	statsPath                    string
}

type tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func text(v any) string           { s, _ := v.(string); return s }
func jwtClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(b, &claims) != nil {
		return nil
	}
	return claims
}
func expiresSoon(token string) bool {
	exp, _ := jwtClaims(token)["exp"].(float64)
	return exp > 0 && time.Unix(int64(exp), 0).Before(time.Now().Add(2*time.Minute))
}
func accountHeaders(h http.Header, token string) {
	auth := object(jwtClaims(token)["https://api.openai.com/auth"])
	if s := text(auth["chatgpt_account_id"]); s != "" {
		h.Set("ChatGPT-Account-ID", s)
	}
	residency := text(auth["chatgpt_data_residency"])
	if residency == "" {
		residency = text(auth["chatgpt_compute_residency"])
	}
	if residency != "" {
		h.Set("x-openai-internal-codex-residency", residency)
	}
}

// token refreshes when needed and keeps the legacy two-argument call shape.
func (g *gateway) token(ctx context.Context, force bool) (string, error) {
	return g.tokenFor(ctx, force, "request")
}

// tokenFor is token() with the trigger recorded, so the log says why a refresh
// happened: a scheduled check, a request, or an upstream 401.
func (g *gateway) tokenFor(ctx context.Context, force bool, reason string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.authPath == "" {
		return "", errors.New("auth path missing")
	}
	lock, err := os.OpenFile(strings.TrimSuffix(g.authPath, filepath.Ext(g.authPath))+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	waited := false
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
		if !waited {
			waited = true
			log.Printf("[refresh] waiting for auth.lock: another process holds it")
		}
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	b, err := os.ReadFile(g.authPath)
	if err != nil {
		return "", err
	}
	var store map[string]any
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	if err = dec.Decode(&store); err != nil {
		return "", fmt.Errorf("invalid auth JSON: %w", err)
	}
	state := object(object(store["providers"])["openai-codex"])
	pair := object(state["tokens"])
	access, refresh := text(pair["access_token"]), text(pair["refresh_token"])
	if access == "" || refresh == "" {
		return "", errors.New("providers.openai-codex.tokens needs access_token and refresh_token")
	}
	if !force && !expiresSoon(access) {
		return access, nil
	}
	endpoint := g.tokenURL
	if endpoint == "" {
		endpoint = defaultTokenURL
	}
	log.Printf("[refresh] start reason=%s force=%t old_fp=%s exp_in=%s expires_at=%s",
		reason, force, tokenFingerprint(access), expiresInText(access), formatOrDash(tokenExpiry(access)))
	started := time.Now()
	body := url.Values{"grant_type": {"refresh_token"}, "client_id": {codexClientID}, "refresh_token": {refresh}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := g.httpClient().Do(req)
	if err != nil {
		log.Printf("[refresh] failed reason=%s dur=%s old_fp=%s err=%v (credentials unchanged)",
			reason, time.Since(started).Round(time.Millisecond), tokenFingerprint(access), err)
		return "", fmt.Errorf("refresh request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Printf("[refresh] failed reason=%s http=%d dur=%s old_fp=%s (credentials unchanged)",
			reason, resp.StatusCode, time.Since(started).Round(time.Millisecond), tokenFingerprint(access))
		return "", fmt.Errorf("refresh failed (HTTP %d); credentials unchanged", resp.StatusCode)
	}
	limited := io.LimitReader(resp.Body, 1<<20)
	var next tokens
	if err = json.NewDecoder(limited).Decode(&next); err != nil {
		log.Printf("[refresh] failed reason=%s http=200 dur=%s old_fp=%s err=%v (credentials unchanged)",
			reason, time.Since(started).Round(time.Millisecond), tokenFingerprint(access), err)
		return "", fmt.Errorf("refresh response: %w", err)
	}
	if next.AccessToken == "" {
		log.Printf("[refresh] failed reason=%s http=200 dur=%s old_fp=%s (response lacks access_token; credentials unchanged)",
			reason, time.Since(started).Round(time.Millisecond), tokenFingerprint(access))
		return "", errors.New("refresh response lacks access_token")
	}
	rotated := next.RefreshToken != ""
	if !rotated {
		next.RefreshToken = refresh
	}
	pair["access_token"], pair["refresh_token"] = next.AccessToken, next.RefreshToken
	state["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	if pool, ok := object(store["credential_pool"])["openai-codex"].([]any); ok {
		for _, v := range pool {
			row := object(v)
			source := text(row["source"])
			if source == "device_code" || source == "manual:device_code" && text(row["access_token"]) == access {
				row["access_token"], row["refresh_token"], row["last_refresh"] = next.AccessToken, next.RefreshToken, state["last_refresh"]
			}
		}
	}
	updated, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return "", err
	}
	if err = atomicSecretWrite(g.authPath, append(updated, '\n')); err != nil {
		log.Printf("[refresh] failed reason=%s http=200 dur=%s rotated=%t (upstream rotated but persist failed; re-authenticate)",
			reason, time.Since(started).Round(time.Millisecond), rotated)
		return "", fmt.Errorf("refreshed upstream but could not persist rotated token; stop and re-authenticate: %w", err)
	}
	log.Printf("[refresh] ok reason=%s http=200 dur=%s rotated=%t new_fp=%s exp_in=%s expires_at=%s last_refresh=%s",
		reason, time.Since(started).Round(time.Millisecond), rotated,
		tokenFingerprint(next.AccessToken), expiresInText(next.AccessToken),
		formatOrDash(tokenExpiry(next.AccessToken)), state["last_refresh"])
	return next.AccessToken, nil
}
func atomicSecretWrite(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".auth-refresh-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (g *gateway) httpClient() *http.Client {
	if g.client != nil {
		return g.client
	}
	return &http.Client{Timeout: 3 * time.Minute}
}
func (g *gateway) authorized(r *http.Request) bool {
	v := r.Header.Get("Authorization")
	if !strings.HasPrefix(v, "Bearer ") {
		return false
	}
	supplied := []byte(strings.TrimPrefix(v, "Bearer "))
	valid := 0
	for _, key := range g.keys {
		valid |= subtle.ConstantTimeCompare(supplied, []byte(key))
	}
	return valid == 1
}
func (g *gateway) upstreamURL() string {
	if g.upstream != "" {
		return strings.TrimSuffix(g.upstream, "/")
	}
	return defaultUpstream
}
func number(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case json.Number:
		f, _ := x.Float64()
		return f
	default:
		return 0
	}
}
func resetTime(v any) string {
	n := number(v)
	if n <= 0 {
		return ""
	}
	if n > 1e12 {
		n /= 1000
	}
	return time.Unix(int64(n), 0).UTC().Format(time.RFC3339)
}
func percent(v any) float64 {
	n := number(v)
	if n < 0 {
		return 0
	}
	if n > 100 {
		return 100
	}
	return n
}
func usageWindow(v any, label string) map[string]any {
	m := object(v)
	used := percent(m["used_percent"])
	if n := number(m["limit_window_seconds"]); n == 18000 {
		label = "session"
	} else if n == 604800 {
		label = "weekly"
	}
	return map[string]any{"label": label, "used_percent": used, "remaining_percent": 100 - used, "reset_at": resetTime(m["reset_at"])}
}
func (g *gateway) usage(w http.ResponseWriter, r *http.Request) {
	token, err := g.token(r.Context(), false)
	if err != nil {
		apiError(w, 502, "credential_error", err.Error())
		return
	}
	endpoint := strings.TrimSuffix(g.upstreamURL(), "/codex") + "/wham/usage"
	req, err := http.NewRequestWithContext(r.Context(), "GET", endpoint, nil)
	if err != nil {
		apiError(w, 500, "internal_error", "invalid upstream URL")
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex-cli")
	accountHeaders(req.Header, token)
	resp, err := g.httpClient().Do(req)
	if err != nil {
		apiError(w, 502, "upstream_error", err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		apiError(w, resp.StatusCode, "upstream_error", fmt.Sprintf("usage upstream HTTP %d", resp.StatusCode))
		return
	}
	var data map[string]any
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data); err != nil {
		apiError(w, 502, "upstream_error", "invalid usage response")
		return
	}
	rate := object(data["rate_limit"])
	result := map[string]any{"provider": "openai-codex", "plan": data["plan_type"], "windows": []any{usageWindow(rate["primary_window"], "session"), usageWindow(rate["secondary_window"], "weekly")}, "banked_resets": number(object(data["rate_limit_reset_credits"])["available_count"])}
	// Named windows make this endpoint convenient for scripts, while preserving upstream ordering above.
	for _, v := range result["windows"].([]any) {
		m := v.(map[string]any)
		result[text(m["label"])] = m
	}
	writeJSON(w, 200, result)
}
func (g *gateway) scheduler(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	log.Printf("[refresh] scheduler started interval=%s", interval)
	lastState := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c, cancel := context.WithTimeout(ctx, 35*time.Second)
			_, err := g.tokenFor(c, false, "scheduler")
			cancel()
			if err != nil {
				log.Printf("[refresh] scheduler check failed: %v", err)
			}
			// Hourly heartbeat so monitoring can tell "quiet" from "stuck";
			// every refresh also logs on its own.
			if time.Since(lastState) >= time.Hour {
				lastState = time.Now()
				g.logCredentialState()
			}
		}
	}
}
