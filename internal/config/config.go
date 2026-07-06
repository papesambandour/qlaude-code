// Package config resolves qlaude runtime configuration from environment
// variables, applying sensible defaults. Everything is overridable so the
// wrapper stays flexible without ever hardcoding machine-specific values.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Mode selects which backend proxy qlaude uses.
type Mode int

const (
	ModeAPI  Mode = iota // copilot-api on port 4141 (Premium quota)
	ModeChat             // vscode-lm-proxy on port 4000 (Chat quota, unlimited)
)

// Config holds every knob qlaude needs to boot the copilot-api proxy and
// launch Claude Code against it.
type Config struct {
	Mode Mode   // ModeAPI or ModeChat
	Host string // interface the proxy listens on
	Port int    // port the proxy listens on

	AutoStart bool // start the proxy automatically when it is down (API mode only)

	CopilotAPICmd string // command used to launch the copilot-api proxy
	ClaudeCmd     string // command used to launch Claude Code

	// Model overrides. When empty, qlaude auto-detects them from the proxy's
	// /v1/models endpoint so it always matches the current Copilot plan.
	// ModelPrimary is the default model Claude Code launches with (ANTHROPIC_MODEL);
	// the others feed Claude Code's per-tier aliases.
	ModelPrimary string
	ModelSonnet  string
	ModelOpus    string
	ModelHaiku   string

	StartTimeout        time.Duration // how long to wait for the proxy to become ready
	DisableNonEssential bool          // set Claude Code flags that avoid non-proxy traffic
	Verbose             bool          // verbose proxy logging + qlaude chatter
	Quiet               bool          // silence qlaude's own stderr messages
}

// Load builds a Config from the current environment (mode is set later by main).
func Load() *Config {
	c := &Config{
		Mode:                ModeAPI,
		Host:                envStr("QLAUDE_HOST", "127.0.0.1"),
		Port:                envInt("QLAUDE_PORT", 4141),
		AutoStart:           !envBool("QLAUDE_NO_AUTOSTART", false),
		CopilotAPICmd:       envStr("QLAUDE_COPILOT_API_CMD", "copilot-api"),
		ClaudeCmd:           envStr("QLAUDE_CLAUDE_CMD", "claude"),
		ModelPrimary:        envStr("QLAUDE_MODEL", ""),
		ModelSonnet:         envStr("QLAUDE_SONNET_MODEL", ""),
		ModelOpus:           envStr("QLAUDE_OPUS_MODEL", ""),
		ModelHaiku:          envStr("QLAUDE_HAIKU_MODEL", ""),
		StartTimeout:        time.Duration(envInt("QLAUDE_START_TIMEOUT", 45)) * time.Second,
		DisableNonEssential: !envBool("QLAUDE_KEEP_NONESSENTIAL", false),
		Verbose:             envBool("QLAUDE_VERBOSE", false),
		Quiet:               envBool("QLAUDE_QUIET", false),
	}
	return c
}

// ApplyChatMode switches the config to Chat mode (vscode-lm-proxy).
// Port defaults to 4000 unless QLAUDE_CHAT_PORT is set.
// AutoStart is always false in Chat mode (VS Code must already be running).
func (c *Config) ApplyChatMode() {
	c.Mode = ModeChat
	c.Port = envInt("QLAUDE_CHAT_PORT", 4000)
	c.AutoStart = false
}

// BaseURL is the Anthropic-compatible endpoint exposed by copilot-api.
func (c *Config) BaseURL() string {
	return fmt.Sprintf("http://%s:%d", c.Host, c.Port)
}

// Addr is the host:port dialed to check whether the proxy is up.
func (c *Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

// Dir is qlaude's state directory (~/.qlaude), created on demand.
func (c *Config) Dir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "qlaude")
	}
	return filepath.Join(home, ".qlaude")
}

// EnsureDir creates the state directory if needed.
func (c *Config) EnsureDir() error {
	return os.MkdirAll(c.Dir(), 0o755)
}

// LogPath is where the detached copilot-api process writes its output.
func (c *Config) LogPath() string { return filepath.Join(c.Dir(), "copilot-api.log") }

// PidPath records the PID of the proxy qlaude started, for later management.
func (c *Config) PidPath() string { return filepath.Join(c.Dir(), "copilot-api.pid") }

// CopilotTokenPath is copilot-api's stored GitHub token. Absence means the
// user still needs to run `copilot-api auth`.
func (c *Config) CopilotTokenPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "share", "copilot-api", "github_token")
}

// EnvInt reads an integer environment variable with a default.
func EnvInt(key string, def int) int { return envInt(key, def) }

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		switch v {
		case "1", "true", "TRUE", "yes", "on":
			return true
		case "0", "false", "FALSE", "no", "off":
			return false
		}
	}
	return def
}
