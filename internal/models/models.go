// Package models discovers which Copilot models are available through the
// proxy and picks sensible defaults for Claude Code's model tiers. Nothing is
// hardcoded to a specific plan: defaults are chosen from the live model list.
package models

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/papesambandour/qlaude-code/internal/config"
)

// Set maps Claude Code's model tiers to concrete Copilot model IDs.
type Set struct {
	Primary string // default model Claude Code launches with (ANTHROPIC_MODEL)
	Sonnet  string // sonnet-tier alias
	Opus    string // opus-tier alias
	Haiku   string // small, fast, background model
}

// primaryDefault is the model qlaude uses by default when nothing is
// overridden and it exists on the current Copilot plan.
const primaryDefault = "claude-opus-4.6"

// Preference lists, most-preferred first. The first ID that exists in the live
// model list wins. Kept broad so qlaude adapts to whatever the plan exposes.
var (
	sonnetPrefs = []string{"claude-sonnet-5", "claude-sonnet-4.6", "claude-sonnet-4.5", "gpt-5.4", "gpt-4.1"}
	opusPrefs   = []string{"claude-opus-4.6", "claude-opus-4.8", "claude-opus-4.7"}
	haikuPrefs  = []string{"claude-haiku-4.5", "gpt-5.4-mini", "gpt-4o-mini", "gpt-4.1"}
)

// Fallbacks used when the model list cannot be fetched.
var fallback = Set{
	Primary: "claude-opus-4.6",
	Sonnet:  "claude-sonnet-5",
	Opus:    "claude-opus-4.6",
	Haiku:   "claude-haiku-4.5",
}

// Fetch returns the model IDs advertised by the proxy's /v1/models endpoint.
func Fetch(baseURL string) ([]string, error) {
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get(baseURL + "/v1/models")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

// Resolve produces the model set for Claude Code. Explicit overrides always
// win; otherwise defaults are picked from the live model list.
func Resolve(baseURL string, c *config.Config) Set {
	ids, err := Fetch(baseURL)
	if err != nil || len(ids) == 0 {
		return withOverrides(fallback, c)
	}

	set := Set{
		Sonnet: pick(ids, sonnetPrefs, ids[0]),
		Opus:   pick(ids, opusPrefs, ""),
		Haiku:  pick(ids, haikuPrefs, ""),
	}
	if set.Opus == "" {
		set.Opus = set.Sonnet
	}
	if set.Haiku == "" {
		set.Haiku = set.Sonnet
	}
	// The default model prefers opus-4.6; if the plan lacks it, fall back to
	// the detected opus tier, then sonnet.
	set.Primary = pick(ids, []string{primaryDefault}, "")
	if set.Primary == "" {
		set.Primary = set.Opus
	}
	if set.Primary == "" {
		set.Primary = set.Sonnet
	}
	return withOverrides(set, c)
}

func withOverrides(s Set, c *config.Config) Set {
	if c.ModelPrimary != "" {
		s.Primary = c.ModelPrimary
	}
	if c.ModelSonnet != "" {
		s.Sonnet = c.ModelSonnet
	}
	if c.ModelOpus != "" {
		s.Opus = c.ModelOpus
	}
	if c.ModelHaiku != "" {
		s.Haiku = c.ModelHaiku
	}
	return s
}

// pick returns the first preference present in ids (exact match, then prefix),
// or def when none match.
func pick(ids, prefs []string, def string) string {
	for _, p := range prefs {
		for _, id := range ids {
			if id == p {
				return id
			}
		}
	}
	for _, p := range prefs {
		for _, id := range ids {
			if strings.HasPrefix(id, p) {
				return id
			}
		}
	}
	return def
}
