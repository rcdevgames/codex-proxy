package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"time"
)

// tokenFingerprint returns a short, non-reversible id for a credential so the
// log can say *which* token is in play and whether it rotated, without ever
// printing the value. Eight hex chars of SHA-256 are plenty to spot a change.
func tokenFingerprint(secret string) string {
	if secret == "" {
		return "none"
	}
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:4])
}

// tokenExpiry reads the exp claim of a JWT; zero time when there is none.
func tokenExpiry(secret string) time.Time {
	exp, _ := jwtClaims(secret)["exp"].(float64)
	if exp <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(exp), 0).UTC()
}

func expiresInText(secret string) string {
	exp := tokenExpiry(secret)
	if exp.IsZero() {
		return "unknown"
	}
	d := time.Until(exp).Round(time.Second)
	if d < 0 {
		return "-" + (-d).String()
	}
	return d.String()
}

// logCredentialState prints the stored credential's fingerprint and remaining
// validity. It reads the file directly so it never triggers a refresh.
func (g *gateway) logCredentialState() {
	b, err := os.ReadFile(g.authPath)
	if err != nil {
		log.Printf("[refresh] state unreadable: %v", err)
		return
	}
	var store map[string]any
	if json.Unmarshal(b, &store) != nil {
		log.Printf("[refresh] state unreadable: invalid auth JSON")
		return
	}
	state := object(object(store["providers"])["openai-codex"])
	access := text(object(state["tokens"])["access_token"])
	log.Printf("[refresh] state fp=%s exp_in=%s expires_at=%s last_refresh=%s",
		tokenFingerprint(access), expiresInText(access), formatOrDash(tokenExpiry(access)), dashIfEmpty(text(state["last_refresh"])))
}

func formatOrDash(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format(time.RFC3339)
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
