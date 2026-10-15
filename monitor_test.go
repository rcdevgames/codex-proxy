package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// captureLog collects log output for a single test and restores the default
// sink afterwards; the rest of the suite must not inherit the buffer.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func TestRefreshLogReportsEventsWithoutSecrets(t *testing.T) {
	oldAccess, oldRefresh := jwtWithExpiry(time.Now().Add(45*time.Second)), "refresh-token-old-value"
	newAccess, newRefresh := jwtWithExpiry(time.Now().Add(time.Hour)), "refresh-token-new-value"
	path := filepath.Join(t.TempDir(), "auth.json")
	store := `{"providers":{"openai-codex":{"tokens":{"access_token":"` + oldAccess + `","refresh_token":"` + oldRefresh + `"}}}}`
	if err := os.WriteFile(path, []byte(store), 0600); err != nil {
		t.Fatal(err)
	}
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": newAccess, "refresh_token": newRefresh})
	}))
	defer tokenSrv.Close()
	g := &gateway{authPath: path, tokenURL: tokenSrv.URL, client: &http.Client{Timeout: time.Second}}

	buf := captureLog(t)
	if _, err := g.tokenFor(context.Background(), true, "test"); err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"[refresh] start reason=test force=true",
		"old_fp=",
		"[refresh] ok reason=test http=200",
		"rotated=true",
		"new_fp=",
		"expires_at=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q; got:\n%s", want, out)
		}
	}
	if tokenFingerprint(oldAccess) == tokenFingerprint(newAccess) {
		t.Error("fingerprints must differ so rotation is visible in the log")
	}
	for _, secret := range []string{oldAccess, newAccess, oldRefresh, newRefresh} {
		if strings.Contains(out, secret) {
			t.Errorf("log leaked a credential (fp=%s):\n%s", tokenFingerprint(secret), out)
		}
	}
	// The rotated token must also be on disk, not only in the log.
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(onDisk), newRefresh) {
		t.Error("rotated refresh token not persisted")
	}
}

func TestRefreshLogReportsFailureWithoutSecrets(t *testing.T) {
	access, refresh := jwtWithExpiry(time.Now().Add(45*time.Second)), "refresh-token-that-must-not-leak"
	path := filepath.Join(t.TempDir(), "auth.json")
	store := `{"providers":{"openai-codex":{"tokens":{"access_token":"` + access + `","refresh_token":"` + refresh + `"}}}}`
	if err := os.WriteFile(path, []byte(store), 0600); err != nil {
		t.Fatal(err)
	}
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","refresh_token":"` + refresh + `"}`))
	}))
	defer tokenSrv.Close()
	g := &gateway{authPath: path, tokenURL: tokenSrv.URL, client: &http.Client{Timeout: time.Second}}

	buf := captureLog(t)
	if _, err := g.tokenFor(context.Background(), true, "test"); err == nil {
		t.Fatal("expected refresh to fail")
	}
	out := buf.String()
	if !strings.Contains(out, "[refresh] failed reason=test http=400") || !strings.Contains(out, "credentials unchanged") {
		t.Errorf("failure not reported:\n%s", out)
	}
	if strings.Contains(out, access) || strings.Contains(out, refresh) {
		t.Errorf("log leaked a credential:\n%s", out)
	}
}

func TestLogCredentialStateIsFingerprintOnly(t *testing.T) {
	access, refresh := jwtWithExpiry(time.Now().Add(time.Hour)), "refresh-token-not-to-be-logged"
	path := filepath.Join(t.TempDir(), "auth.json")
	store := `{"providers":{"openai-codex":{"tokens":{"access_token":"` + access + `","refresh_token":"` + refresh + `"},"last_refresh":"2026-10-14T09:00:00Z"}}}`
	if err := os.WriteFile(path, []byte(store), 0600); err != nil {
		t.Fatal(err)
	}
	g := &gateway{authPath: path, client: &http.Client{Timeout: time.Second}}

	buf := captureLog(t)
	g.logCredentialState()
	out := buf.String()
	for _, want := range []string{"[refresh] state fp=", "expires_at=", "last_refresh=2026-10-14T09:00:00Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, access) || strings.Contains(out, refresh) {
		t.Errorf("log leaked a credential:\n%s", out)
	}
}

func TestTokenFingerprintIsStableAndShort(t *testing.T) {
	a, b := tokenFingerprint("value-one"), tokenFingerprint("value-one")
	if a != b || a == tokenFingerprint("value-two") {
		t.Errorf("fingerprint not stable/unique: %s %s", a, b)
	}
	if len(a) != 8 {
		t.Errorf("fingerprint=%q want 8 hex chars", a)
	}
	if tokenFingerprint("") != "none" {
		t.Error("empty secret should report none")
	}
}
