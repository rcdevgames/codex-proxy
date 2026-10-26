package main

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

//go:embed monitor.html
var monitorHTML string

var monitorTpl = template.Must(template.New("monitor").Parse(monitorHTML))

// monitorAuthed checks the session cookie set after a successful secret match.
// The cookie value is a fixed marker; the gate is that it is only issued when the
// secret matched, and the page binds to loopback/TLS proxy anyway.
func (g *gateway) monitorAuthed(r *http.Request) bool {
	if g.monitorSecret == "" {
		return false
	}
	c, err := r.Cookie("monitor_ok")
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte("1")) == 1
}

// protectMonitor is the routing gate for guarded monitor routes. It runs in the
// mux (before the handler), so protected handlers only ever see authed requests.
func (g *gateway) protectMonitor(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if g.monitorSecret == "" {
			http.Error(w, "monitor secret not configured", http.StatusServiceUnavailable)
			return
		}
		if !g.monitorAuthed(r) {
			http.Redirect(w, r, "/monitor", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

// monitorLogin renders the login page (GET) or verifies the secret (POST).
func (g *gateway) monitorLogin(w http.ResponseWriter, r *http.Request) {
	if g.monitorSecret == "" {
		http.Error(w, "monitor secret not configured", http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodPost {
		supplied := []byte(r.FormValue("secret"))
		expected := []byte(g.monitorSecret)
		if subtle.ConstantTimeCompare(supplied, expected) != 1 {
			renderLogin(w, "Secret key salah.")
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: "monitor_ok", Value: "1", Path: "/monitor",
			HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 86400,
		})
		http.Redirect(w, r, "/monitor/dashboard", http.StatusSeeOther)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !g.monitorAuthed(r) {
		renderLogin(w, "")
		return
	}
	http.Redirect(w, r, "/monitor/dashboard", http.StatusSeeOther)
}

// monitorLogout clears the session cookie and returns to the login page.
func (g *gateway) monitorLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "monitor_ok", Value: "", Path: "/monitor", MaxAge: -1})
	http.Redirect(w, r, "/monitor", http.StatusSeeOther)
}

// monitorDashboard renders the dashboard. Only reachable via protectMonitor.
func (g *gateway) monitorDashboard(w http.ResponseWriter, r *http.Request) {
	data := g.monitorData(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := monitorTpl.Execute(w, data); err != nil {
		// template already wrote partial output; nothing to do
	}
}

// monitorAPI is the JSON endpoint the dashboard polls every 3 seconds.
func (g *gateway) monitorAPI(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, g.monitorData(r))
}

// monitorReset clears all usage stats and history (dashboard reset button).
func (g *gateway) monitorReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := g.usageStats.reset(); err != nil {
		apiError(w, 500, "internal_error", err.Error())
		return
	}
	log.Printf("[monitor] usage stats reset")
	http.Redirect(w, r, "/monitor/dashboard", http.StatusSeeOther)
}

// fetchPoolUsage queries the same upstream usage endpoint as /usages and
// returns the normalized result for the dashboard's pool panel.
func (g *gateway) fetchPoolUsage(token string) (map[string]any, error) {
	endpoint := strings.TrimSuffix(g.upstreamURL(), "/codex") + "/wham/usage"
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream URL")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex-cli")
	accountHeaders(req.Header, token)
	resp, err := g.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("usage upstream HTTP %d", resp.StatusCode)
	}
	var data map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data); err != nil {
		return nil, fmt.Errorf("invalid usage response")
	}
	rate := object(data["rate_limit"])
	result := map[string]any{
		"provider": "openai-codex",
		"plan":     data["plan_type"],
		"windows":  []any{usageWindow(rate["primary_window"], "session"), usageWindow(rate["secondary_window"], "weekly")},
	}
	return result, nil
}

func renderLogin(w http.ResponseWriter, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(200)
	_, _ = io.WriteString(w, strings.Replace(loginPageHTML, "%s", errMsg, 1))
}

type monitorRow struct {
	Key        string
	Requests   int64
	Prompt     int64
	Completion int64
	Total      int64
	LastUsed   string
	Share      float64
}

type monitorData struct {
	Rows      []monitorRow
	TotalReqs int64
	TotalTok  int64
	TopKey    string
	TopTokens int64
	TopShare  float64
	Pool      map[string]any
	PoolErr   string
	HasPool   bool
	History   []usageEvent
	DayTokens int64
	DayReqs   int64
	SevenTok  int64
	MonthTok  int64
	NinetyTok int64
}

func (g *gateway) monitorPoolData(r *http.Request) (map[string]any, bool, string) {
	tok, err := g.token(r.Context(), false)
	if err != nil {
		return nil, false, err.Error()
	}
	pool, err := g.fetchPoolUsage(tok)
	if err != nil {
		return nil, false, err.Error()
	}
	return pool, true, ""
}

// monitorData assembles the dashboard data: per-key stats (masked) and pool usage
// from upstream. Metadata only; never request/response bodies.
func (g *gateway) monitorData(r *http.Request) monitorData {
	var data monitorData
	data.Pool = map[string]any{}
	ranked := g.usageStats.ranked()
	for _, e := range ranked {
		data.TotalReqs += e.Stat.Requests
		data.TotalTok += e.Stat.TotalTokens
	}
	for _, e := range ranked {
		share := 0.0
		if data.TotalTok > 0 {
			share = float64(e.Stat.TotalTokens) / float64(data.TotalTok) * 100
		}
		data.Rows = append(data.Rows, monitorRow{
			Key:        maskKey(e.Key),
			Requests:   e.Stat.Requests,
			Prompt:     e.Stat.PromptTokens,
			Completion: e.Stat.CompletionTokens,
			Total:      e.Stat.TotalTokens,
			LastUsed:   e.Stat.LastUsed.UTC().Format("2006-01-02 15:04:05"),
			Share:      share,
		})
	}
	if len(ranked) > 0 {
		data.TopKey = maskKey(ranked[0].Key)
		data.TopTokens = ranked[0].Stat.TotalTokens
		if data.TotalTok > 0 {
			data.TopShare = float64(ranked[0].Stat.TotalTokens) / float64(data.TotalTok) * 100
		}
	}
	data.Pool, data.HasPool, data.PoolErr = g.monitorPoolData(r)
	// History events leave the gateway with masked keys only.
	now := time.Now().UTC()
	for _, e := range g.usageStats.events() {
		e.Key = maskKey(e.Key)
		// Aggregate token totals per period for the summary cards.
		t, err := time.Parse(time.RFC3339, e.At)
		if err == nil {
			age := now.Sub(t)
			tokens := e.Prompt + e.Completion
			if age <= 24*time.Hour {
				data.DayTokens += tokens
				data.DayReqs++
			}
			if age <= 7*24*time.Hour {
				data.SevenTok += tokens
			}
			if age <= 30*24*time.Hour {
				data.MonthTok += tokens
			}
			if age <= 90*24*time.Hour {
				data.NinetyTok += tokens
			}
		}
		data.History = append(data.History, e)
	}
	return data
}

const loginPageHTML = `<!doctype html>
<html><head><meta charset="utf-8"><title>Monitor Login</title>
<style>body{font-family:sans-serif;background:#111;color:#eee;display:flex;justify-content:center;padding-top:15vh}form{background:#1c1c1c;padding:2rem;border-radius:8px;min-width:320px}input{width:100%;padding:10px;margin:12px 0;background:#222;border:1px solid #444;color:#eee;border-radius:4px;box-sizing:border-box}button{width:100%;padding:10px;background:#4a6cf7;color:#fff;border:0;border-radius:4px;cursor:pointer}.err{color:#f66;font-size:14px}</style>
</head><body><div style="font-family:sans-serif;background:#111;color:#eee;display:flex;justify-content:center;padding-top:15vh"><form method="POST" action="/monitor"><h3>Codex Bridge Monitor</h3><p class="err">%s</p><input type="password" name="secret" placeholder="Secret key" autofocus required><button>Login</button></form></div></body></html>`
