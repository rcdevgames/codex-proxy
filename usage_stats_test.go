package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMaskKey(t *testing.T) {
	key := "abcdef1234567890xyz"
	got := maskKey(key)
	if !strings.HasPrefix(got, key[:5]) {
		t.Fatalf("prefix: %s", got)
	}
	if !strings.HasSuffix(got, key[len(key)-3:]) {
		t.Fatalf("suffix: %s", got)
	}
	if strings.Contains(got, key[5:len(key)-3]) {
		t.Fatalf("middle leaked: %s", got)
	}
	if len([]rune(got)) != maskVisible {
		t.Fatalf("length changed: %s (%d != %d)", got, len([]rune(got)), maskVisible)
	}
	// Short keys never reveal length or content.
	short := maskKey("tiny")
	if strings.Contains(short, "tiny") || len([]rune(short)) != maskVisible {
		t.Fatalf("short key leak: %q", short)
	}
}

func TestUsageTrackerRecordAndRank(t *testing.T) {
	tr := newUsageTracker("")
	tr.record("key-a", 0, 0)
	tr.record("key-a", 100, 40)
	tr.addTokens("key-a", 10, 5)
	tr.record("key-b", 0, 0)
	tr.addTokens("key-b", 5, 1)

	r := tr.ranked()
	if len(r) != 2 {
		t.Fatalf("len=%d", len(r))
	}
	if r[0].Key != "key-a" || r[0].Stat.TotalTokens != 100+40+10+5 {
		t.Fatalf("top=%+v", r[0])
	}
	if r[1].Key != "key-b" || r[1].Stat.Requests != 1 {
		t.Fatalf("second=%+v", r[1])
	}
	var nilTr *usageTracker
	nilTr.record("x", 1, 1)
}

func TestMonitorAuthAndMasking(t *testing.T) {
	tr := newUsageTracker("")
	tr.record("abcdef1234567890keyXy123", 0, 0)
	tr.addTokens("abcdef1234567890keyXy123", 30, 20)
	tr.addEvent(usageEvent{Key: "abcdef1234567890keyXy123", Endpoint: "/v1/chat/completions", Model: "gpt-6-sol", Prompt: 30, Completion: 20})
	g := &gateway{monitorSecret: "sekret-rahasia", usageStats: tr}

	// No cookie -> login page at /monitor.
	req := httptest.NewRequest("GET", "/monitor", nil)
	rec := httptest.NewRecorder()
	g.monitorLogin(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Monitor Login") {
		t.Fatalf("login: %d", rec.Code)
	}

	// Protected route without cookie -> redirect to /monitor.
	req = httptest.NewRequest("GET", "/monitor/dashboard", nil)
	rec = httptest.NewRecorder()
	g.protectMonitor(g.monitorDashboard)(rec, req)
	if rec.Code != 303 || rec.Header().Get("Location") != "/monitor" {
		t.Fatalf("protect redirect: %d %s", rec.Code, rec.Header().Get("Location"))
	}

	// Wrong secret -> login page (200 with error, not authed).
	req = httptest.NewRequest("POST", "/monitor", strings.NewReader("secret=salah"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	g.monitorLogin(rec, req)
	if !strings.Contains(rec.Body.String(), "Secret key salah") {
		t.Fatalf("wrong secret: %s", rec.Body.String())
	}

	// Right secret -> redirect to dashboard + cookie.
	req = httptest.NewRequest("POST", "/monitor", strings.NewReader("secret=sekret-rahasia"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	g.monitorLogin(rec, req)
	if rec.Code != 303 || rec.Header().Get("Location") != "/monitor/dashboard" {
		t.Fatalf("good secret: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "monitor_ok" {
			session = c
		}
	}
	if session == nil {
		t.Fatal("no session cookie")
	}

	// Authed GET /monitor -> redirect to dashboard.
	req = httptest.NewRequest("GET", "/monitor", nil)
	req.AddCookie(session)
	rec = httptest.NewRecorder()
	g.monitorLogin(rec, req)
	if rec.Code != 303 || rec.Header().Get("Location") != "/monitor/dashboard" {
		t.Fatalf("authed login redirect: %d", rec.Code)
	}

	// Authed dashboard renders skeleton initially (data via SSE).
	req = httptest.NewRequest("GET", "/monitor/dashboard", nil)
	req.AddCookie(session)
	rec = httptest.NewRecorder()
	g.monitorDashboard(rec, req)
	if rec.Code != 200 {
		t.Fatalf("dashboard: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Codex Bridge Monitor") {
		t.Fatalf("missing title: %s", rec.Body.String()[:200])
	}
	// SSE stream: verify monitorData masks the key and includes history.
	req = httptest.NewRequest("GET", "/monitor/sse", nil)
	req.AddCookie(session)
	data := g.monitorData(req)
	found := false
	for _, e := range data.History {
		if strings.HasPrefix(e.Key, "abcde") && !strings.Contains(e.Key, "234567890123") && strings.HasSuffix(e.Key, "123") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("history mask wrong: %+v", data.History)
	}
	if data.TotalTok != 50 {
		t.Fatalf("total: %d", data.TotalTok)
	}
}

func TestMonitorJSONUnauthorized(t *testing.T) {
	g := &gateway{monitorSecret: "x", usageStats: newUsageTracker("")}
	rec := httptest.NewRecorder()
	g.protectMonitor(g.monitorAPI)(rec, httptest.NewRequest("GET", "/monitor/json", nil))
	if rec.Code != 303 {
		t.Fatalf("status=%d", rec.Code)
	}
}
