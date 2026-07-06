// Package acpproxy implements an Anthropic-compatible HTTP proxy backed by the
// GitHub Copilot CLI via the ACP (Agent Client Protocol) over stdin/stdout.
//
// This bypasses the copilot-api Premium quota entirely — the Copilot CLI uses
// its own session-based quota which is separate and much more generous.
//
// Flow:
//   qlaude --acp  →  local HTTP :4002  →  copilot --acp subprocess (JSON-RPC/NDJSON)
//                                                 ↓
//                                     GitHub Copilot (CLI session quota)
package acpproxy

import (
	"bufio"
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
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/papesambandour/qlaude-code/internal/config"
)

// ---------------------------------------------------------------------------
// ACP JSON-RPC types (minimal)
// ---------------------------------------------------------------------------

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ---------------------------------------------------------------------------
// ACP session (one Copilot CLI subprocess per proxy instance)
// ---------------------------------------------------------------------------

type session struct {
	mu        sync.Mutex
	proc      *exec.Cmd
	stdin     io.WriteCloser
	scanner   *bufio.Scanner
	nextID    atomic.Int32
	sessionID string
	ready     chan struct{}
	pending   map[int]chan rpcMsg
	updates   map[int]chan rpcMsg // streaming updates for a request
}

func newSession(copilotBin string, cwd string) (*session, error) {
	cmd := exec.Command(copilotBin, "--acp", "--allow-all", "--add-dir", cwd)
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start copilot --acp: %w", err)
	}

	s := &session{
		proc:    cmd,
		stdin:   stdin,
		scanner: bufio.NewScanner(stdout),
		ready:   make(chan struct{}),
		pending: make(map[int]chan rpcMsg),
		updates: make(map[int]chan rpcMsg),
	}
	s.scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	go s.readLoop()

	// initialize
	if err := s.initialize(); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("acp initialize: %w", err)
	}
	// create persistent session
	if err := s.newCopilotSession(cwd); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("acp session/new: %w", err)
	}
	close(s.ready)
	return s, nil
}

func (s *session) readLoop() {
	for s.scanner.Scan() {
		line := s.scanner.Bytes()
		var msg rpcMsg
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if msg.ID != nil {
			s.mu.Lock()
			ch := s.pending[*msg.ID]
			uch := s.updates[*msg.ID]
			s.mu.Unlock()
			if ch != nil {
				select {
				case ch <- msg:
				default:
				}
			}
			if uch != nil {
				select {
				case uch <- msg:
				default:
				}
			}
			continue
		}
		// Notification (no id) — route agent_message_chunk and stop to update channels
		if msg.Method == "session/update" {
			var params struct {
				SessionID string `json:"sessionId"`
				Update    struct {
					SessionUpdate string          `json:"sessionUpdate"`
					Content       json.RawMessage `json:"content"`
					StopReason    string          `json:"stopReason"`
				} `json:"update"`
			}
			if json.Unmarshal(msg.Params, &params) == nil {
				su := params.Update.SessionUpdate
				if su == "agent_message_chunk" || su == "stop" {
					// find the pending update channel by looking for the most recent prompt
					s.mu.Lock()
					for _, uch := range s.updates {
						select {
						case uch <- msg:
						default:
						}
					}
					s.mu.Unlock()
				}
			}
		}
	}
}

func (s *session) send(msg rpcMsg) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = fmt.Fprintf(s.stdin, "%s\n", data)
	return err
}

func (s *session) call(method string, params interface{}) (rpcMsg, error) {
	id := int(s.nextID.Add(1))
	var raw json.RawMessage
	if params != nil {
		b, _ := json.Marshal(params)
		raw = b
	}
	ch := make(chan rpcMsg, 1)
	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()

	idPtr := id
	if err := s.send(rpcMsg{JSONRPC: "2.0", ID: &idPtr, Method: method, Params: raw}); err != nil {
		return rpcMsg{}, err
	}
	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(30 * time.Second):
		return rpcMsg{}, fmt.Errorf("timeout waiting for %s response", method)
	}
}

func (s *session) initialize() error {
	r, err := s.call("initialize", map[string]interface{}{
		"protocolVersion": 1,
		"clientInfo":      map[string]string{"name": "qlaude", "version": "1.0"},
	})
	if err != nil {
		return err
	}
	if r.Error != nil {
		return fmt.Errorf("initialize error: %s", r.Error.Message)
	}
	return nil
}

func (s *session) newCopilotSession(cwd string) error {
	// register update channel for id=2 before sending
	id := int(s.nextID.Add(1))
	ch := make(chan rpcMsg, 1)
	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()

	params, _ := json.Marshal(map[string]interface{}{
		"cwd":        cwd,
		"mcpServers": []interface{}{},
	})
	if err := s.send(rpcMsg{JSONRPC: "2.0", ID: &id, Method: "session/new", Params: params}); err != nil {
		return err
	}

	// Drain notifications until we get the actual result
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case resp := <-ch:
			if resp.Error != nil {
				return fmt.Errorf("session/new error: %s", resp.Error.Message)
			}
			if resp.Result != nil {
				var result struct {
					SessionID string `json:"sessionId"`
				}
				if err := json.Unmarshal(resp.Result, &result); err == nil && result.SessionID != "" {
					s.sessionID = result.SessionID
					return nil
				}
			}
		case <-time.After(deadline.Sub(time.Now())):
			return fmt.Errorf("timeout waiting for session/new result")
		}
	}
	return fmt.Errorf("no sessionId received")
}

// prompt sends a user message and collects the streamed response.
func (s *session) prompt(text string) (string, error) {
	<-s.ready // wait until initialized

	id := int(s.nextID.Add(1))
	resultCh := make(chan rpcMsg, 1)
	updateCh := make(chan rpcMsg, 64)

	s.mu.Lock()
	s.pending[id] = resultCh
	s.updates[id] = updateCh
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		delete(s.updates, id)
		s.mu.Unlock()
	}()

	params, _ := json.Marshal(map[string]interface{}{
		"sessionId": s.sessionID,
		"prompt":    []map[string]string{{"type": "text", "text": text}},
	})
	if err := s.send(rpcMsg{JSONRPC: "2.0", ID: &id, Method: "session/prompt", Params: params}); err != nil {
		return "", err
	}

	var sb strings.Builder
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case msg := <-updateCh:
			if msg.ID != nil && *msg.ID == id {
				// final result
				return sb.String(), nil
			}
			// notification
			if msg.Method == "session/update" {
				var params struct {
					Update struct {
						SessionUpdate string `json:"sessionUpdate"`
						Content       struct {
							Type string `json:"type"`
							Text string `json:"text"`
						} `json:"content"`
						StopReason string `json:"stopReason"`
					} `json:"update"`
				}
				if json.Unmarshal(msg.Params, &params) == nil {
					switch params.Update.SessionUpdate {
					case "agent_message_chunk":
						if params.Update.Content.Type == "text" {
							sb.WriteString(params.Update.Content.Text)
						}
					case "stop":
						return sb.String(), nil
					}
				}
			}
		case msg := <-resultCh:
			if msg.Error != nil {
				return "", fmt.Errorf("session/prompt: %s", msg.Error.Message)
			}
			return sb.String(), nil
		case <-time.After(time.Until(deadline)):
			return sb.String(), fmt.Errorf("timeout")
		}
	}
	return sb.String(), nil
}

func (s *session) stop() {
	_ = s.stdin.Close()
	if s.proc != nil && s.proc.Process != nil {
		_ = s.proc.Process.Signal(syscall.SIGTERM)
	}
}

// ---------------------------------------------------------------------------
// HTTP proxy server
// ---------------------------------------------------------------------------

var globalSession *session
var sessionMu sync.Mutex

func getOrCreateSession(copilotBin, cwd string) (*session, error) {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	if globalSession != nil {
		return globalSession, nil
	}
	s, err := newSession(copilotBin, cwd)
	if err != nil {
		return nil, err
	}
	globalSession = s
	return s, nil
}

func copilotBinPath() string {
	if p, err := exec.LookPath("copilot"); err == nil {
		return p
	}
	return "copilot"
}

func handler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/models" || r.URL.Path == "/models" {
		serveModels(w)
		return
	}
	if r.URL.Path != "/v1/messages" {
		http.NotFound(w, r)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Extract text from Anthropic messages payload
	text, err := extractPromptText(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	cwd, _ := os.Getwd()
	sess, err := getOrCreateSession(copilotBinPath(), cwd)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{"message": "copilot ACP startup error: " + err.Error(), "type": "server_error"},
		})
		return
	}

	resp, err := sess.prompt(text)
	if err != nil {
		// Session might be dead; reset and retry once
		sessionMu.Lock()
		globalSession = nil
		sessionMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{"message": "ACP error: " + err.Error(), "type": "server_error"},
		})
		return
	}

	out := buildAnthropicResponse(resp)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func extractPromptText(data []byte) (string, error) {
	var payload struct {
		System   json.RawMessage   `json:"system"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", err
	}
	var parts []string
	if len(payload.System) > 0 && string(payload.System) != "null" {
		parts = append(parts, extractText(payload.System))
	}
	for _, m := range payload.Messages {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(m, &msg) == nil {
			t := extractText(msg.Content)
			if t != "" {
				parts = append(parts, fmt.Sprintf("[%s]: %s", msg.Role, t))
			}
		}
	}
	return strings.Join(parts, "\n"), nil
}

func extractText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
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

func buildAnthropicResponse(text string) map[string]interface{} {
	return map[string]interface{}{
		"id":    fmt.Sprintf("msg_acp_%d", time.Now().UnixNano()),
		"type":  "message",
		"role":  "assistant",
		"model": "copilot-cli",
		"content": []map[string]interface{}{
			{"type": "text", "text": text},
		},
		"stop_reason": "end_turn",
		"usage":       map[string]int{"input_tokens": 0, "output_tokens": 0},
	}
}

func serveModels(w http.ResponseWriter) {
	models := []map[string]interface{}{
		{"id": "copilot-cli", "object": "model"},
		{"id": "claude-sonnet-4.6", "object": "model"},
		{"id": "claude-opus-4.8", "object": "model"},
	}
	out, _ := json.Marshal(map[string]interface{}{"data": models, "object": "list"})
	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}

// ---------------------------------------------------------------------------
// Lifecycle helpers (called from main.go)
// ---------------------------------------------------------------------------

func IsRunning(c *config.Config) bool {
	conn, err := net.DialTimeout("tcp", c.Addr(), 600*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

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
	return fmt.Errorf("acp proxy not ready on %s within %s", c.BaseURL(), timeout)
}

func Start(c *config.Config) error {
	if err := os.MkdirAll(c.Dir(), 0o755); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(acpLogPath(c), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(self, "--qlaude", "acp-serve", "--port", strconv.Itoa(c.Port))
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.SysProcAttr = detachSysProcAttr()
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	_ = os.WriteFile(acpPidPath(c), []byte(strconv.Itoa(pid)), 0o644)
	_ = cmd.Process.Release()
	return WaitReady(c, c.StartTimeout)
}

func Stop(c *config.Config) error {
	sessionMu.Lock()
	if globalSession != nil {
		globalSession.stop()
		globalSession = nil
	}
	sessionMu.Unlock()

	data, err := os.ReadFile(acpPidPath(c))
	if err == nil {
		if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && pid > 0 {
			p, _ := os.FindProcess(pid)
			if p != nil {
				_ = p.Signal(syscall.SIGTERM)
			}
		}
		os.Remove(acpPidPath(c))
	}
	return nil
}

// Serve starts the HTTP server in-process (called as daemon subprocess).
func Serve(port int) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	mux := http.NewServeMux()
	mux.HandleFunc("/", handler)
	srv := &http.Server{Addr: addr, Handler: mux, ReadTimeout: 120 * time.Second, WriteTimeout: 120 * time.Second}
	return srv.ListenAndServe()
}

func acpLogPath(c *config.Config) string {
	return filepath.Join(c.Dir(), "acp-proxy.log")
}
func acpPidPath(c *config.Config) string {
	return filepath.Join(c.Dir(), "acp-proxy.pid")
}
