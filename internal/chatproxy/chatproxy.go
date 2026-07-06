// Package chatproxy implements a standalone Anthropic-compatible HTTP proxy
// that forwards requests to api.githubcopilot.com using the VS Code Claude Code
// extension headers (openai-intent: messages-proxy).
//
// It reads the same GitHub token that copilot-api stores, exchanges it for a
// Copilot token, and exposes /v1/messages on a configurable port so Claude Code
// can connect without any external npm dependency.
package chatproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/papesambandour/qlaude-code/internal/config"
)

const (
	copilotTokenURL  = "https://api.github.com/copilot_internal/v2/token"
	copilotChatURL   = "https://api.githubcopilot.com/chat/completions"
	editorVersion    = "vscode/1.100.0"
	pluginVersion    = "copilot-chat/0.52.0"
	claudeCodeAgent  = "vscode_claude_code/2.1.112 (external, sdk-ts, agent-sdk/0.2.112)"
)

// tokenCache holds the Copilot bearer token and its expiry.
type tokenCache struct {
	token     string
	expiresAt time.Time
}

var cache tokenCache

// githubTokenPath returns the path where copilot-api stores the GitHub OAuth token.
func githubTokenPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "copilot-api", "github_token")
}

// readGitHubToken reads the stored GitHub OAuth token.
func readGitHubToken() (string, error) {
	data, err := os.ReadFile(githubTokenPath())
	if err != nil {
		return "", fmt.Errorf("github token not found at %s — run: copilot-api auth", githubTokenPath())
	}
	return strings.TrimSpace(string(data)), nil
}

// fetchCopilotToken exchanges the GitHub token for a short-lived Copilot bearer token.
func fetchCopilotToken(githubToken string) (string, time.Time, error) {
	req, _ := http.NewRequest("GET", copilotTokenURL, nil)
	req.Header.Set("Authorization", "token "+githubToken)
	req.Header.Set("Editor-Version", editorVersion)
	req.Header.Set("Editor-Plugin-Version", pluginVersion)
	req.Header.Set("User-Agent", "GitHubCopilotChat/0.52.0")
	req.Header.Set("X-GitHub-Api-Version", "2026-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", time.Time{}, fmt.Errorf("copilot token exchange failed (%d): %s", resp.StatusCode, body)
	}
	var result struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", time.Time{}, fmt.Errorf("parse copilot token: %w", err)
	}
	exp := time.Unix(result.ExpiresAt, 0)
	return result.Token, exp, nil
}

// copilotToken returns a valid Copilot token, refreshing if needed.
func copilotToken() (string, error) {
	if cache.token != "" && time.Now().Before(cache.expiresAt.Add(-60*time.Second)) {
		return cache.token, nil
	}
	gh, err := readGitHubToken()
	if err != nil {
		return "", err
	}
	tok, exp, err := fetchCopilotToken(gh)
	if err != nil {
		return "", err
	}
	cache = tokenCache{token: tok, expiresAt: exp}
	return tok, nil
}

// buildCopilotHeaders returns the HTTP headers sent to api.githubcopilot.com.
// These mirror the VS Code Claude Code extension's headers so requests are
// routed as "messages-proxy" interactions.
func buildCopilotHeaders(token string, isAgentCall bool) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/json")
	h.Set("Editor-Version", editorVersion)
	h.Set("Editor-Plugin-Version", pluginVersion)
	h.Set("User-Agent", claudeCodeAgent)
	h.Set("Openai-Intent", "messages-proxy")
	h.Set("X-Interaction-Type", "messages-proxy")
	h.Set("X-GitHub-Api-Version", "2026-06-01")
	h.Set("Copilot-Integration-Id", "vscode-chat")
	if isAgentCall {
		h.Set("X-Initiator", "agent") // free — tool call follow-ups don't burn quota
	} else {
		h.Set("X-Initiator", "user") // counts as 1 interaction
	}
	return h
}

// isAgentCall detects whether a request is a tool-call follow-up so we can
// set X-Initiator: agent (which is free).
func isAgentCall(body []byte) bool {
	var payload struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.Messages) == 0 {
		return false
	}
	last := payload.Messages[len(payload.Messages)-1].Role
	return last == "assistant" || last == "tool"
}

// handler proxies /v1/messages requests to api.githubcopilot.com.
func handler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && (r.URL.Path == "/v1/models" || r.URL.Path == "/models") {
		serveModels(w)
		return
	}
	if r.URL.Path != "/v1/messages" {
		http.NotFound(w, r)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	tok, err := copilotToken()
	if err != nil {
		http.Error(w, `{"error":{"message":"`+err.Error()+`","type":"auth_error"}}`, http.StatusUnauthorized)
		return
	}

	// Convert Anthropic /v1/messages payload to OpenAI chat/completions format
	openAIBody, stream, err := anthropicToOpenAI(body)
	if err != nil {
		http.Error(w, `{"error":{"message":"`+err.Error()+`","type":"invalid_request_error"}}`, http.StatusBadRequest)
		return
	}

	req, _ := http.NewRequest("POST", copilotChatURL, bytes.NewReader(openAIBody))
	req.Header = buildCopilotHeaders(tok, isAgentCall(body))
	req.Header.Set("Accept", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, `{"error":{"message":"`+err.Error()+`","type":"connection_error"}}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)

	if stream {
		// Forward SSE stream as Anthropic SSE
		forwardStream(w, respBody)
		return
	}

	// Convert OpenAI response to Anthropic format
	out, err := openAIToAnthropic(respBody)
	if err != nil {
		w.Write(respBody) // fallback: forward raw
		return
	}
	w.Write(out)
}

// anthropicToOpenAI converts an Anthropic Messages API request body to
// OpenAI Chat Completions format.
func anthropicToOpenAI(data []byte) ([]byte, bool, error) {
	var in struct {
		Model     string            `json:"model"`
		MaxTokens int               `json:"max_tokens"`
		Stream    bool              `json:"stream"`
		System    json.RawMessage   `json:"system"` // string or [{type:text,text:...}]
		Messages  []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(data, &in); err != nil {
		return nil, false, err
	}

	msgs := make([]map[string]interface{}, 0, len(in.Messages)+1)

	// system can be a plain string or an array of content blocks
	if len(in.System) > 0 && string(in.System) != "null" {
		systemText := extractText(in.System)
		if systemText != "" {
			msgs = append(msgs, map[string]interface{}{"role": "system", "content": systemText})
		}
	}

	for _, m := range in.Messages {
		msgs = append(msgs, normalizeMessage(m))
	}

	out := map[string]interface{}{
		"model":      in.Model,
		"messages":   msgs,
		"max_tokens": in.MaxTokens,
		"stream":     in.Stream,
	}
	b, err := json.Marshal(out)
	return b, in.Stream, err
}

// extractText pulls a plain string from either a JSON string or an array of
// Anthropic content blocks [{type:"text", text:"..."}].
func extractText(raw json.RawMessage) string {
	// try plain string first
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// try array of content blocks
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Type == "text" {
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}
	return ""
}

// normalizeMessage converts an Anthropic message (content may be string or
// array of blocks) into an OpenAI-compatible message map.
func normalizeMessage(raw json.RawMessage) map[string]interface{} {
	var m struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		var fallback map[string]interface{}
		json.Unmarshal(raw, &fallback)
		return fallback
	}
	text := extractText(m.Content)
	return map[string]interface{}{"role": m.Role, "content": text}
}

// openAIToAnthropic converts an OpenAI Chat Completions response to Anthropic format.
func openAIToAnthropic(data []byte) ([]byte, error) {
	var in struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Usage   struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &in); err != nil {
		return nil, err
	}
	if in.Error != nil {
		errOut := map[string]interface{}{
			"error": map[string]string{
				"message": in.Error.Message,
				"code":    in.Error.Code,
				"type":    "api_error",
			},
		}
		return json.Marshal(errOut)
	}

	text := ""
	stopReason := "end_turn"
	if len(in.Choices) > 0 {
		text = in.Choices[0].Message.Content
		if in.Choices[0].FinishReason == "length" {
			stopReason = "max_tokens"
		}
	}

	out := map[string]interface{}{
		"id":    in.ID,
		"type":  "message",
		"role":  "assistant",
		"model": in.Model,
		"content": []map[string]interface{}{
			{"type": "text", "text": text},
		},
		"stop_reason": stopReason,
		"usage": map[string]int{
			"input_tokens":  in.Usage.PromptTokens,
			"output_tokens": in.Usage.CompletionTokens,
		},
	}
	return json.Marshal(out)
}

// forwardStream converts OpenAI SSE events to Anthropic SSE format (basic).
func forwardStream(w http.ResponseWriter, data []byte) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				fmt.Fprintf(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				break
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if err := json.Unmarshal([]byte(payload), &chunk); err == nil && len(chunk.Choices) > 0 {
				text := chunk.Choices[0].Delta.Content
				if text != "" {
					delta, _ := json.Marshal(map[string]interface{}{
						"type":  "content_block_delta",
						"index": 0,
						"delta": map[string]string{"type": "text_delta", "text": text},
					})
					fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\n", delta)
				}
			}
		}
	}
}

// serveModels returns a minimal Anthropic-compatible models list.
func serveModels(w http.ResponseWriter) {
	models := []map[string]interface{}{
		{"id": "claude-sonnet-5", "object": "model"},
		{"id": "claude-opus-4.6", "object": "model"},
		{"id": "claude-haiku-4.5", "object": "model"},
		{"id": "gpt-4o", "object": "model"},
		{"id": "gpt-4.1", "object": "model"},
	}
	out, _ := json.Marshal(map[string]interface{}{"data": models, "object": "list"})
	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}

func rawToMap(raw json.RawMessage) map[string]interface{} {
	var m map[string]interface{}
	json.Unmarshal(raw, &m)
	return m
}

// IsRunning reports whether the chat proxy is already listening.
func IsRunning(c *config.Config) bool {
	conn, err := net.DialTimeout("tcp", c.Addr(), 600*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// WaitReady polls until the proxy answers on /v1/models.
func WaitReady(c *config.Config, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(c.BaseURL() + "/v1/models")
		if err == nil && resp.StatusCode < 500 {
			resp.Body.Close()
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("chat proxy did not become ready on %s within %s", c.BaseURL(), timeout)
}

// Start launches the chat proxy as a detached background daemon.
func Start(c *config.Config) error {
	if err := os.MkdirAll(c.Dir(), 0o755); err != nil {
		return err
	}

	if _, err := os.Stat(githubTokenPath()); err != nil {
		return fmt.Errorf("copilot-api not authenticated — run: copilot-api auth")
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot find qlaude executable: %w", err)
	}

	logFile, err := os.OpenFile(chatLogPath(c), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open chat proxy log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(self, "--qlaude", "chat-serve", "--port", strconv.Itoa(c.Port))
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.SysProcAttr = detachSysProcAttr()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start chat proxy: %w", err)
	}
	pid := cmd.Process.Pid
	_ = os.WriteFile(chatPidPath(c), []byte(strconv.Itoa(pid)), 0o644)
	_ = cmd.Process.Release()

	return WaitReady(c, c.StartTimeout)
}

// Serve starts the HTTP server in-process (called when qlaude runs as daemon).
func Serve(port int) error {
	addr := "127.0.0.1:" + strconv.Itoa(port)
	mux := http.NewServeMux()
	mux.HandleFunc("/", handler)
	srv := &http.Server{Addr: addr, Handler: mux, ReadTimeout: 120 * time.Second, WriteTimeout: 120 * time.Second}
	return srv.ListenAndServe()
}

// Stop kills the chat proxy daemon.
func Stop(c *config.Config) error {
	data, err := os.ReadFile(chatPidPath(c))
	if err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			p, _ := os.FindProcess(pid)
			if p != nil {
				_ = p.Signal(syscall.SIGTERM)
			}
		}
		os.Remove(chatPidPath(c))
	}
	return nil
}

func chatLogPath(c *config.Config) string { return filepath.Join(c.Dir(), "chat-proxy.log") }
func chatPidPath(c *config.Config) string { return filepath.Join(c.Dir(), "chat-proxy.pid") }
