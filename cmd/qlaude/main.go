// qlaude is a thin wrapper around Claude Code (`claude`) that transparently
// powers it with GitHub Copilot models through a local proxy.
//
// Two proxy backends are available:
//
//	qlaude --api  [claude args]   copilot-api on :4141 (Premium quota, auto-started)
//	qlaude --chat [claude args]   built-in chat proxy on :4000 (auto-started, no VS Code needed)
//	qlaude        [claude args]   defaults to --api
//
// Management commands live behind the reserved `--qlaude` prefix:
//
//	qlaude --qlaude status|start|stop|restart|logs|doctor|env|version|help|uninstall
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/papesambandour/qlaude-code/internal/acpproxy"
	"github.com/papesambandour/qlaude-code/internal/chatproxy"
	"github.com/papesambandour/qlaude-code/internal/config"
	"github.com/papesambandour/qlaude-code/internal/models"
	"github.com/papesambandour/qlaude-code/internal/proxy"
)

// version is set at build time via -ldflags "-X main.version=..."
var version = "0.1.1"

func main() {
	cfg := config.Load()
	args := os.Args[1:]

	// --qlaude management commands (including internal chat-serve)
	if len(args) > 0 && args[0] == "--qlaude" {
		os.Exit(runManagement(cfg, args[1:]))
	}

	// --chat / --api mode flags: consume the flag, leave remaining args for claude
	args = parseMode(cfg, args)

	// re-check for --qlaude after mode flags (e.g. qlaude --chat --qlaude status)
	if len(args) > 0 && args[0] == "--qlaude" {
		os.Exit(runManagement(cfg, args[1:]))
	}

	runWrapper(cfg, args)
}

// parseMode scans args for --chat or --api, applies the mode to cfg, and
// returns args with those flags removed so they are never forwarded to claude.
func parseMode(cfg *config.Config, args []string) []string {
	out := args[:0:0] // empty slice, same backing array capacity
	for _, a := range args {
		switch a {
		case "--chat":
			cfg.ApplyChatMode()
		case "--acp":
			cfg.ApplyACPMode()
		case "--api":
			// explicit API mode — already the default, nothing to change
		default:
			out = append(out, a)
		}
	}
	return out
}

// runWrapper ensures the proxy is up, wires the environment, and hands over to
// Claude Code via exec (replacing this process so the TUI, signals and TTY all
// behave exactly as a native `claude` invocation).
func runWrapper(cfg *config.Config, args []string) {
	claudePath, err := exec.LookPath(cfg.ClaudeCmd)
	if err != nil {
		fatalf("%q not found in PATH.\nInstall Claude Code: https://docs.anthropic.com/en/docs/claude-code/overview", cfg.ClaudeCmd)
	}

	modeLabel := "api"
	if cfg.Mode == config.ModeChat {
		modeLabel = "chat"
	} else if cfg.Mode == config.ModeACP {
		modeLabel = "acp (copilot cli)"
	}
	infof(cfg, "mode=%s  proxy=%s", modeLabel, cfg.BaseURL())

	if err := ensureProxy(cfg); err != nil {
		handleProxyError(cfg, err)
	}

	set := models.Resolve(cfg.BaseURL(), cfg)
	infof(cfg, "model=%s  (sonnet=%s opus=%s haiku=%s)", set.Primary, set.Sonnet, set.Opus, set.Haiku)

	env := buildEnv(cfg, set)
	argv := append([]string{cfg.ClaudeCmd}, args...)

	if err := syscall.Exec(claudePath, argv, env); err != nil {
		fatalf("failed to exec %s: %v", claudePath, err)
	}
}

// ensureProxy ensures the right proxy is running for the selected mode.
func ensureProxy(cfg *config.Config) error {
	switch cfg.Mode {
	case config.ModeACP:
		if acpproxy.IsRunning(cfg) {
			if acpproxy.WaitReady(cfg, 3*time.Second) == nil {
				infof(cfg, "acp proxy ready on %s", cfg.BaseURL())
				return nil
			}
		}
		infof(cfg, "starting copilot ACP proxy on %s ...", cfg.BaseURL())
		return acpproxy.Start(cfg)

	case config.ModeChat:
		if chatproxy.IsRunning(cfg) {
			if chatproxy.WaitReady(cfg, 3*time.Second) == nil {
				infof(cfg, "chat proxy ready on %s", cfg.BaseURL())
				return nil
			}
		}
		infof(cfg, "starting built-in chat proxy on %s ...", cfg.BaseURL())
		return chatproxy.Start(cfg)

	default: // ModeAPI
		if proxy.IsRunning(cfg) {
			if proxy.Ready(cfg) {
				infof(cfg, "copilot-api ready on %s", cfg.BaseURL())
				return nil
			}
			return proxy.WaitReady(cfg, cfg.StartTimeout)
		}
		if !cfg.AutoStart {
			return fmt.Errorf("copilot-api is not running on %s and autostart is disabled", cfg.BaseURL())
		}
		infof(cfg, "starting copilot-api on %s ...", cfg.BaseURL())
		return proxy.Start(cfg)
	}
}

// handleProxyError prints an actionable message and exits.
func handleProxyError(cfg *config.Config, err error) {
	if err == proxy.ErrAuthMissing || strings.Contains(err.Error(), "not authenticated") {
		fatalf("copilot-api is not authenticated.\nRun once:  copilot-api auth\nThen retry your qlaude command.")
	}
	if cfg.Mode == config.ModeChat {
		fatalf("could not start chat proxy: %v\nCheck logs: %s", err, cfg.LogPath())
	}
	fatalf("could not start copilot-api: %v\nCheck logs: %s", err, cfg.LogPath())
}

// buildEnv returns the process environment with the ANTHROPIC_* overrides that
// point Claude Code at the local Copilot proxy.
func buildEnv(cfg *config.Config, set models.Set) []string {
	overrides := map[string]string{
		"ANTHROPIC_BASE_URL":             cfg.BaseURL(),
		"ANTHROPIC_AUTH_TOKEN":           "dummy",
		"ANTHROPIC_MODEL":                set.Primary,
		"ANTHROPIC_DEFAULT_SONNET_MODEL": set.Sonnet,
		"ANTHROPIC_DEFAULT_OPUS_MODEL":   set.Opus,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":  set.Haiku,
		"ANTHROPIC_SMALL_FAST_MODEL":     set.Haiku,
	}
	if cfg.DisableNonEssential {
		overrides["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] = "1"
		overrides["DISABLE_NON_ESSENTIAL_MODEL_CALLS"] = "1"
	}
	return mergeEnv(os.Environ(), overrides)
}

// mergeEnv overlays overrides onto base, replacing existing keys in place.
func mergeEnv(base []string, overrides map[string]string) []string {
	out := make([]string, 0, len(base)+len(overrides))
	seen := make(map[string]bool, len(overrides))
	for _, kv := range base {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if v, ok := overrides[key]; ok {
			out = append(out, key+"="+v)
			seen[key] = true
			continue
		}
		out = append(out, kv)
	}
	for k, v := range overrides {
		if !seen[k] {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Management commands (behind the reserved `--qlaude` prefix)
// ---------------------------------------------------------------------------

func runManagement(cfg *config.Config, args []string) int {
	sub := "help"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "status":
		return cmdStatus(cfg)
	case "start":
		return cmdStart(cfg)
	case "stop":
		return cmdStop(cfg)
	case "restart":
		return cmdRestart(cfg)
	case "logs":
		return cmdLogs(cfg, args[1:])
	case "doctor":
		return cmdDoctor(cfg)
	case "env":
		return cmdEnv(cfg)
	case "uninstall":
		return cmdUninstall(cfg)
	case "acp-serve": // internal: run ACP proxy server in-process
		port := config.EnvInt("QLAUDE_ACP_PORT", 4002)
		for i, a := range args[1:] {
			if a == "--port" && i+1 < len(args)-1 {
				if p, err := strconv.Atoi(args[i+2]); err == nil {
					port = p
				}
			}
		}
		if err := acpproxy.Serve(port); err != nil {
			fmt.Fprintln(os.Stderr, "acp proxy error:", err)
			return 1
		}
		return 0
	case "chat-serve": // internal: run built-in chat proxy server in-process
		port := config.EnvInt("QLAUDE_CHAT_PORT", 4000)
		for i, a := range args[1:] {
			if a == "--port" && i+1 < len(args)-1 {
				if p, err := strconv.Atoi(args[i+2]); err == nil {
					port = p
				}
			}
		}
		if err := chatproxy.Serve(port); err != nil {
			fmt.Fprintln(os.Stderr, "chat proxy error:", err)
			return 1
		}
		return 0
	case "version", "--version", "-v":
		fmt.Printf("qlaude %s\n", version)
		return 0
	case "help", "--help", "-h":
		printHelp()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "qlaude: unknown command %q\n\n", sub)
		printHelp()
		return 2
	}
}

func cmdStatus(cfg *config.Config) int {
	mode := "api (copilot-api, Premium quota)"
	if cfg.Mode == config.ModeChat {
		mode = "chat (built-in proxy, copilot-api HTTP)"
	} else if cfg.Mode == config.ModeACP {
		mode = "acp (Copilot CLI subprocess — CLI session quota)"
	}
	running := proxy.IsRunning(cfg)
	ready := running && proxy.Ready(cfg)
	fmt.Printf("mode      : %s\n", mode)
	fmt.Printf("proxy url : %s\n", cfg.BaseURL())
	fmt.Printf("running   : %s\n", yesno(running))
	fmt.Printf("ready     : %s\n", yesno(ready))
	if pid := readPid(cfg); pid != "" {
		fmt.Printf("pid file  : %s (%s)\n", pid, cfg.PidPath())
	}
	if ready {
		set := models.Resolve(cfg.BaseURL(), cfg)
		fmt.Printf("model     : %s\n", set.Primary)
		fmt.Printf("tiers     : sonnet=%s opus=%s haiku=%s\n", set.Sonnet, set.Opus, set.Haiku)
	}
	return 0
}

func cmdStart(cfg *config.Config) int {
	if proxy.IsRunning(cfg) {
		fmt.Printf("copilot-api already running on %s\n", cfg.BaseURL())
		return 0
	}
	fmt.Printf("starting copilot-api on %s ...\n", cfg.BaseURL())
	if err := proxy.Start(cfg); err != nil {
		if err == proxy.ErrAuthMissing {
			fmt.Fprintln(os.Stderr, "copilot-api is not authenticated. Run: copilot-api auth")
			return 1
		}
		fmt.Fprintf(os.Stderr, "failed: %v\nlogs: %s\n", err, cfg.LogPath())
		return 1
	}
	fmt.Printf("ready on %s\n", cfg.BaseURL())
	return 0
}

func cmdStop(cfg *config.Config) int {
	switch cfg.Mode {
	case config.ModeACP:
		if err := acpproxy.Stop(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "stop error: %v\n", err)
			return 1
		}
		fmt.Println("acp proxy stopped")
	case config.ModeChat:
		if err := chatproxy.Stop(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "stop error: %v\n", err)
			return 1
		}
		fmt.Println("chat proxy stopped")
	default:
		stopped, err := proxy.Stop(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "stop error: %v\n", err)
			return 1
		}
		if !stopped {
			fmt.Println("no running copilot-api found")
		} else {
			fmt.Println("copilot-api stopped")
		}
	}
	return 0
}

func cmdRestart(cfg *config.Config) int {
	_, _ = proxy.Stop(cfg)
	return cmdStart(cfg)
}

func cmdLogs(cfg *config.Config, args []string) int {
	path := cfg.LogPath()
	if len(args) > 0 && (args[0] == "-f" || args[0] == "--follow") {
		tail, err := exec.LookPath("tail")
		if err == nil {
			c := exec.Command(tail, "-f", path)
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr
			_ = c.Run()
			return 0
		}
	}
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "no logs at %s (%v)\n", path, err)
		return 1
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	const max = 200
	if len(lines) > max {
		lines = lines[len(lines)-max:]
	}
	for _, l := range lines {
		fmt.Println(l)
	}
	return 0
}

func cmdEnv(cfg *config.Config) int {
	set := models.Resolve(cfg.BaseURL(), cfg)
	overrides := buildEnv(cfg, set)
	for _, kv := range overrides {
		if strings.HasPrefix(kv, "ANTHROPIC_") ||
			strings.HasPrefix(kv, "CLAUDE_CODE_") ||
			strings.HasPrefix(kv, "DISABLE_NON_ESSENTIAL_") {
			fmt.Printf("export %s\n", kv)
		}
	}
	return 0
}

func cmdUninstall(cfg *config.Config) int {
	// 1. stop the proxy if running
	if proxy.IsRunning(cfg) {
		fmt.Println("stopping copilot-api proxy...")
		if _, err := proxy.Stop(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not stop proxy: %v\n", err)
		} else {
			fmt.Println("proxy stopped")
		}
	}

	// 2. remove the state directory (~/.qlaude/)
	stateDir := cfg.Dir()
	if _, err := os.Stat(stateDir); err == nil {
		if err := os.RemoveAll(stateDir); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not remove %s: %v\n", stateDir, err)
		} else {
			fmt.Printf("removed state dir  %s\n", stateDir)
		}
	}

	// 3. find and remove the qlaude binary
	self, err := exec.LookPath("qlaude")
	if err != nil {
		// fall back to the currently running executable
		self, _ = os.Executable()
	}
	if self != "" {
		if err := os.Remove(self); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not remove binary %s: %v\n", self, err)
			fmt.Fprintf(os.Stderr, "  remove it manually: rm -f %s\n", self)
			return 1
		}
		fmt.Printf("removed binary     %s\n", self)
	}

	fmt.Println("qlaude uninstalled.")
	return 0
}

func cmdDoctor(cfg *config.Config) int {
	fmt.Println("qlaude doctor")
	fmt.Println(strings.Repeat("-", 40))
	checkBin("node")
	checkBin(cfg.ClaudeCmd)
	checkBin(cfg.CopilotAPICmd)

	if tok := cfg.CopilotTokenPath(); tok != "" {
		if _, err := os.Stat(tok); err == nil {
			fmt.Printf("[ok]   copilot-api authenticated (%s)\n", tok)
		} else {
			fmt.Printf("[warn] copilot-api not authenticated — run: copilot-api auth\n")
		}
	}

	running := proxy.IsRunning(cfg)
	ready := running && proxy.Ready(cfg)
	fmt.Printf("[%s] proxy %s (running=%s ready=%s)\n", okmark(ready), cfg.BaseURL(), yesno(running), yesno(ready))
	if ready {
		if ids, err := models.Fetch(cfg.BaseURL()); err == nil {
			fmt.Printf("[ok]   %d models available\n", len(ids))
		}
		set := models.Resolve(cfg.BaseURL(), cfg)
		fmt.Printf("[ok]   default model=%s (sonnet=%s opus=%s haiku=%s)\n", set.Primary, set.Sonnet, set.Opus, set.Haiku)
	} else {
		fmt.Printf("[info] start it with: qlaude --qlaude start  (or just run qlaude)\n")
	}
	return 0
}

func printHelp() {
	fmt.Print(`qlaude — Claude Code powered by GitHub Copilot

USAGE
  qlaude [--api|--chat] [claude args...]

MODES
  (default) qlaude [args]        --api mode: copilot-api proxy :4141, auto-started
  qlaude --api  [args]           copilot-api proxy :4141 (Premium quota, auto-started)
  qlaude --chat [args]           Built-in chat proxy :4000 (auto-started, no VS Code needed)
  qlaude --acp  [args]           Copilot CLI ACP subprocess :4002 (CLI session quota, no API limit!)

EXAMPLES
  qlaude                         API mode, interactive Claude Code
  qlaude --acp                   ACP mode via Copilot CLI (uses CLI session, not API quota)
  qlaude --chat                  Chat mode
  qlaude --acp  -p "hello"       ACP mode, one-shot prompt
  qlaude --api  -p "hello"       API mode, one-shot prompt

MANAGEMENT (reserved --qlaude prefix, works with --chat and --api)
  qlaude [--chat|--api] --qlaude status     Show proxy status and models
  qlaude [--chat|--api] --qlaude start      Start the proxy
  qlaude [--chat|--api] --qlaude stop       Stop the proxy
  qlaude [--chat|--api] --qlaude restart    Restart the proxy
  qlaude --qlaude logs [-f]                 Show (or follow) proxy logs
  qlaude --qlaude env                       Print the env vars qlaude exports
  qlaude --qlaude doctor                    Diagnose the setup
  qlaude --qlaude uninstall                 Stop proxies, remove state and binary
  qlaude --qlaude version                   Print qlaude version

ENVIRONMENT
  QLAUDE_PORT (4141)             API proxy port
  QLAUDE_CHAT_PORT (4000)        Chat proxy port
  QLAUDE_HOST (127.0.0.1)        Proxy host
  QLAUDE_MODEL                   Override default model
  QLAUDE_SONNET_MODEL            Override sonnet-tier model
  QLAUDE_OPUS_MODEL              Override opus-tier model
  QLAUDE_HAIKU_MODEL             Override haiku/fast model
  QLAUDE_NO_AUTOSTART=1          Disable API proxy auto-start
  QLAUDE_START_TIMEOUT (45)      Seconds to wait for proxy to be ready
  QLAUDE_VERBOSE=1               Verbose output
  QLAUDE_QUIET=1                 Silence qlaude's own messages
`)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func infof(cfg *config.Config, format string, a ...any) {
	if cfg.Quiet {
		return
	}
	fmt.Fprintf(os.Stderr, "qlaude: "+format+"\n", a...)
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "qlaude: "+format+"\n", a...)
	os.Exit(1)
}

func checkBin(name string) {
	if p, err := exec.LookPath(name); err == nil {
		fmt.Printf("[ok]   %-12s %s\n", name, p)
	} else {
		fmt.Printf("[fail] %-12s not found in PATH\n", name)
	}
}

func readPid(cfg *config.Config) string {
	data, err := os.ReadFile(cfg.PidPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func yesno(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func okmark(b bool) string {
	if b {
		return "ok"
	}
	return "warn"
}
