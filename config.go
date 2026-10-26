package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type config struct {
	Host                string   `yaml:"host"`
	Port                int      `yaml:"port"`
	APIKeys             []string `yaml:"api_keys"`
	RefreshCheckSeconds int      `yaml:"refresh_check_seconds"`
	MonitorSecret       string   `yaml:"monitor_secret"`
}

func loadConfig(configPath, binaryPath string) (config, string, error) {
	var cfg config
	path, err := filepath.Abs(configPath)
	if err != nil {
		return cfg, "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return cfg, "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return cfg, "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return cfg, "", errors.New("config.yaml must be a regular file accessible only to its owner (mode 0600)")
	}
	dec := yaml.NewDecoder(file)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return cfg, "", fmt.Errorf("invalid config.yaml: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return cfg, "", errors.New("config.yaml must contain one YAML document")
	} else if !errors.Is(err, io.EOF) {
		return cfg, "", fmt.Errorf("invalid config.yaml: %w", err)
	}
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	if cfg.Host != "127.0.0.1" && cfg.Host != "localhost" && cfg.Host != "::1" {
		return cfg, "", errors.New("only loopback host is allowed; use TLS reverse proxy for remote access")
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return cfg, "", errors.New("port must be between 1 and 65535")
	}
	if len(cfg.APIKeys) == 0 {
		return cfg, "", errors.New("api_keys must contain at least one key")
	}
	for _, key := range cfg.APIKeys {
		if len(key) < 16 || strings.TrimSpace(key) != key {
			return cfg, "", errors.New("every api_keys entry must have at least 16 characters and no surrounding whitespace")
		}
	}
	if cfg.RefreshCheckSeconds == 0 {
		cfg.RefreshCheckSeconds = 60
	}
	if cfg.RefreshCheckSeconds < 0 {
		return cfg, "", errors.New("refresh_check_seconds must be positive")
	}
	bin, err := filepath.Abs(binaryPath)
	if err != nil {
		return cfg, "", err
	}
	for _, dir := range []string{filepath.Dir(path), filepath.Dir(bin)} {
		candidate := filepath.Join(dir, "auth.json")
		canonical, err := filepath.EvalSymlinks(candidate)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return cfg, "", fmt.Errorf("resolve auth.json: %w", err)
		}
		info, err := os.Stat(canonical)
		if err != nil {
			return cfg, "", err
		}
		if !info.Mode().IsRegular() {
			return cfg, "", fmt.Errorf("auth.json is not a regular file: %s", candidate)
		}
		return cfg, canonical, nil
	}
	return cfg, "", errors.New("auth.json must be alongside config.yaml or the binary")
}
func (c config) address() string { return net.JoinHostPort(c.Host, fmt.Sprint(c.Port)) }
