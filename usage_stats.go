package main

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// keyStat holds per-gateway-API-key usage counters. Only aggregates are kept:
// no request or response bodies, no prompts, no model payloads.
type keyStat struct {
	Requests         int64     `json:"requests"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	TotalTokens      int64     `json:"total_tokens"`
	FirstUsed        time.Time `json:"first_used"`
	LastUsed         time.Time `json:"last_used"`
}

// usageTracker accumulates per-key stats and persists them so numbers survive
// restarts. The file contains only counts and masked-able fingerprints; the
// raw keys are stored because the dashboard needs stable identity per key, and
// the file lives at 0600 next to config.yaml.
type usageTracker struct {
	mu      sync.Mutex
	path    string
	dirty   bool
	stats   map[string]*keyStat
	history []usageEvent
}

func newUsageTracker(path string) *usageTracker {
	t := &usageTracker{path: path, stats: map[string]*keyStat{}}
	t.load()
	return t
}

// statsFile is the persisted shape of usage-stats.json.
type statsFile struct {
	Stats   map[string]*keyStat `json:"stats"`
	History []usageEvent        `json:"history"`
}

func (t *usageTracker) load() {
	b, err := os.ReadFile(t.path)
	if err != nil {
		return
	}
	var stored statsFile
	if err := json.Unmarshal(b, &stored); err != nil || stored.Stats == nil {
		// legacy format: bare map of stats without history
		var legacy map[string]*keyStat
		if json.Unmarshal(b, &legacy) == nil {
			t.stats = legacy
		}
	} else {
		t.stats = stored.Stats
	}
	if t.stats == nil {
		t.stats = map[string]*keyStat{}
	}
	// Drop any null entries that a legacy write may have left behind.
	for k, s := range t.stats {
		if s == nil {
			delete(t.stats, k)
		}
	}
	if stored.History != nil {
		t.history = stored.History
	}
}

// record bumps counters for one request. token counts may be zero when the
// endpoint cannot know them (image generations, responses before completion).
func (t *usageTracker) record(key string, prompt, completion int64) {
	if t == nil || key == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.stats[key]
	if !ok {
		s = &keyStat{FirstUsed: time.Now().UTC()}
		t.stats[key] = s
	}
	s.Requests++
	s.PromptTokens += prompt
	s.CompletionTokens += completion
	s.TotalTokens = s.PromptTokens + s.CompletionTokens
	s.LastUsed = time.Now()
	t.dirty = true
}

// addTokens bumps only token totals for an already-recorded request, so the
// outer handler counts the request once and handlers add token usage when the
// upstream reports it (usage arrives after the request completes).
func (t *usageTracker) addTokens(key string, prompt, completion int64) {
	if t == nil || key == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.stats[key]
	if !ok {
		s = &keyStat{FirstUsed: time.Now().UTC()}
		t.stats[key] = s
	}
	if prompt > 0 {
		s.PromptTokens += prompt
	}
	if completion > 0 {
		s.CompletionTokens += completion
	}
	s.TotalTokens = s.PromptTokens + s.CompletionTokens
	if s.LastUsed.IsZero() {
		s.LastUsed = time.Now()
	}
	t.dirty = true
}

// usageEvent is one recorded API hit. Metadata only: masked-able key, model
// name, token counts and timings — never request/response bodies.
type usageEvent struct {
	At         string `json:"at"`
	Key        string `json:"key"`
	Endpoint   string `json:"endpoint"`
	Model      string `json:"model"`
	Prompt     int64  `json:"prompt_tokens"`
	Completion int64  `json:"completion_tokens"`
	Cached     int64  `json:"cached_tokens"`
	TTFTms     int64  `json:"ttft_ms"`
	Durationms int64  `json:"duration_ms"`
}

// maxHistory caps the in-memory and on-disk event history.
const maxHistory = 500

// addEvent appends one hit to the history ring, trimming the oldest.
func (t *usageTracker) addEvent(e usageEvent) {
	if t == nil || e.Key == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.history = append(t.history, e)
	if len(t.history) > maxHistory {
		t.history = t.history[len(t.history)-maxHistory:]
	}
	t.dirty = true
}

// reset clears all stats and history (the dashboard's reset button).
func (t *usageTracker) reset() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stats = map[string]*keyStat{}
	t.history = nil
	t.dirty = true
	return t.flushLocked()
}

// history returns the newest events last.
func (t *usageTracker) events() []usageEvent {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]usageEvent, len(t.history))
	copy(out, t.history)
	return out
}

// flush writes the stats file when something changed; called periodically.
func (t *usageTracker) flush() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.flushLocked()
}

func (t *usageTracker) flushLocked() error {
	if !t.dirty {
		return nil
	}
	b, err := json.MarshalIndent(statsFile{Stats: t.stats, History: t.history}, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicSecretWrite(t.path, append(b, '\n')); err != nil {
		return err
	}
	t.dirty = false
	return nil
}

// ranked returns stats sorted by total tokens, descending.
func (t *usageTracker) ranked() []struct {
	Key  string
	Stat keyStat
} {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]struct {
		Key  string
		Stat keyStat
	}, 0, len(t.stats))
	for k, s := range t.stats {
		out = append(out, struct {
			Key  string
			Stat keyStat
		}{k, *s})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Stat.TotalTokens > out[j].Stat.TotalTokens })
	return out
}

// requestKey returns the gateway API key of the request for usage attribution.
func (g *gateway) requestKey(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// maskKey shows the first 5 and last 3 characters and hides the middle with a
// short fixed-width filler. Every key renders to the same length, so the true
// key length (which would identify the key) is not visible.
const maskVisible = 5 + 4 + 3

func maskKey(key string) string {
	if len(key) <= 8 {
		return strings.Repeat("•", maskVisible)
	}
	return key[:5] + strings.Repeat("•", 4) + key[len(key)-3:]
}
