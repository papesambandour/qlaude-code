// Package proxy manages the lifecycle of the copilot-api server that exposes
// GitHub Copilot as an Anthropic-compatible endpoint for Claude Code.
package proxy

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/papesambandour/qlaude-code/internal/config"
)

// ErrAuthMissing signals that copilot-api has no stored GitHub token, so the
// user must run `copilot-api auth` before the proxy can serve requests.
var ErrAuthMissing = errors.New("copilot-api is not authenticated")

// IsRunning reports whether something is already listening on the proxy port.
func IsRunning(c *config.Config) bool {
	conn, err := net.DialTimeout("tcp", c.Addr(), 600*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Ready performs a lightweight HTTP probe against the models endpoint to make
// sure the proxy is not just bound but actually serving.
func Ready(c *config.Config) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(c.BaseURL() + "/v1/models")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 500
}

// WaitReady polls until the proxy answers or the timeout elapses.
func WaitReady(c *config.Config, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if Ready(c) {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("copilot-api did not become ready on %s within %s", c.BaseURL(), timeout)
}

// Start launches copilot-api as a detached background daemon and waits for it
// to become ready. The process survives after qlaude hands control to Claude
// Code, and keeps running until explicitly stopped.
func Start(c *config.Config) error {
	if err := c.EnsureDir(); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	bin, err := exec.LookPath(c.CopilotAPICmd)
	if err != nil {
		return fmt.Errorf("%q not found in PATH — install it with `npm install -g copilot-api`", c.CopilotAPICmd)
	}

	if tok := c.CopilotTokenPath(); tok != "" {
		if _, statErr := os.Stat(tok); statErr != nil {
			return ErrAuthMissing
		}
	}

	logFile, err := os.OpenFile(c.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	defer logFile.Close()

	args := []string{"start", "--port", strconv.Itoa(c.Port)}
	if c.Verbose {
		args = append(args, "--verbose")
	}

	cmd := exec.Command(bin, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	// Detach into its own session so it outlives the qlaude process.
	cmd.SysProcAttr = detachProcAttr()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start copilot-api: %w", err)
	}

	pid := cmd.Process.Pid
	_ = os.WriteFile(c.PidPath(), []byte(strconv.Itoa(pid)), 0o644)
	// We do not wait on the daemon; release it so it is fully independent.
	_ = cmd.Process.Release()

	if err := WaitReady(c, c.StartTimeout); err != nil {
		return err
	}
	return nil
}

// Stop terminates the proxy. It first tries the PID qlaude recorded, then falls
// back to whatever process is listening on the port.
func Stop(c *config.Config) (stopped bool, err error) {
	var lastErr error
	killed := false

	if data, readErr := os.ReadFile(c.PidPath()); readErr == nil {
		if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil {
			if kerr := killPID(pid); kerr == nil {
				killed = true
			} else {
				lastErr = kerr
			}
		}
		_ = os.Remove(c.PidPath())
	}

	for _, pid := range listenerPIDs(c.Port) {
		if kerr := killPID(pid); kerr == nil {
			killed = true
		} else {
			lastErr = kerr
		}
	}

	if killed {
		return true, nil
	}
	if lastErr != nil {
		return false, lastErr
	}
	return false, nil
}
