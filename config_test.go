package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadYAMLConfigMultipleKeysAndColocatedAuth(t *testing.T) {
	dir := t.TempDir()
	binaryDir := filepath.Join(dir, "bin")
	configDir := filepath.Join(dir, "settings")
	for _, p := range []string{binaryDir, configDir} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	realAuth := filepath.Join(dir, "hermes-auth.json")
	if err := os.WriteFile(realAuth, []byte(`{"providers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realAuth, filepath.Join(configDir, "auth.json")); err != nil {
		t.Fatal(err)
	}
	cfgFile := filepath.Join(configDir, "config.yaml")
	data := []byte("host: 127.0.0.1\nport: 8765\napi_keys:\n  - first-key-0123456789\n  - second-key-0123456789\nrefresh_check_seconds: 60\n")
	if err := os.WriteFile(cfgFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, authPath, err := loadConfig(cfgFile, filepath.Join(binaryDir, "codex-bridge"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "127.0.0.1" || cfg.Port != 8765 || cfg.RefreshCheckSeconds != 60 || len(cfg.APIKeys) != 2 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if authPath != realAuth {
		t.Fatalf("auth path=%q, want canonical %q", authPath, realAuth)
	}
}
func TestGatewayAcceptsEveryConfiguredKey(t *testing.T) {
	g := &gateway{keys: []string{"first-key-0123456789", "second-key-0123456789"}}
	for _, tc := range []struct {
		key  string
		want bool
	}{{"first-key-0123456789", true}, {"second-key-0123456789", true}, {"wrong-key-0123456789", false}, {"", false}} {
		req := httptest.NewRequest("GET", "/usages", nil)
		req.Header.Set("Authorization", "Bearer "+tc.key)
		if got := g.authorized(req); got != tc.want {
			t.Errorf("key %q authorized=%v, want %v", tc.key, got, tc.want)
		}
	}
}
func TestLoadYAMLConfigBinaryAuthFallback(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	cfgDir := filepath.Join(dir, "conf")
	for _, p := range []string{binDir, cfgDir} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(cfgDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("host: 127.0.0.1\nport: 7777\napi_keys: [a-long-enough-api-key]\nrefresh_check_seconds: 60\n"), 0600); err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(binDir, "auth.json")
	if err := os.WriteFile(authPath, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, got, err := loadConfig(cfgPath, filepath.Join(binDir, "gateway"))
	if err != nil {
		t.Fatal(err)
	}
	if got != authPath {
		t.Fatalf("auth path=%q", got)
	}
}
func TestLoadYAMLConfigRejectsAbsentAuth(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("host: 127.0.0.1\nport: 8765\napi_keys: [a-long-enough-api-key]\nrefresh_check_seconds: 60\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadConfig(cfgPath, filepath.Join(t.TempDir(), "gateway")); err == nil {
		t.Fatal("expected missing colocated auth.json to fail")
	}
}
func TestLoadYAMLConfigRejectsUnknownFieldsAndBadKeys(t *testing.T) {
	for _, body := range []string{
		"host: 127.0.0.1\nport: 8765\napi_keys: [a-long-enough-api-key]\nupstream_url: http://evil\n",
		"host: 127.0.0.1\nport: 8765\napi_keys: [a-long-enough-api-key]\n---\n: malformed\n",
		"host: 127.0.0.1\nport: 8765\napi_keys: [short]\n",
		"host: 127.0.0.1\nport: 0\napi_keys: [a-long-enough-api-key]\n",
	} {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(cfgPath, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadConfig(cfgPath, filepath.Join(dir, "gateway")); err == nil {
			t.Fatalf("expected rejection for config with fields: %q", body)
		}
	}
}
