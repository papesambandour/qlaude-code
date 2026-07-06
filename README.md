# qlaude

**Claude Code, powered by GitHub Copilot.**

`qlaude` is a thin Go wrapper around [Claude Code](https://docs.anthropic.com/en/docs/claude-code/overview) (`claude`). When you run it, it:

1. **Ensures the [`copilot-api`](https://github.com/ericc-ch/copilot-api) proxy is running** on the machine — starting it automatically as a background daemon if it's down.
2. **Exports the `ANTHROPIC_*` environment variables** that point Claude Code at the local Copilot proxy.
3. **Execs `claude`, forwarding every argument untouched** — `qlaude <anything>` behaves exactly like `claude <anything>`.

So instead of `claude`, you just type `qlaude`.

---

## Prerequisites

| Tool | Install | Notes |
|------|---------|-------|
| `node` | https://nodejs.org | Runtime for the proxy |
| `claude` (Claude Code) | https://docs.anthropic.com/en/docs/claude-code/overview | The CLI being wrapped |
| `copilot-api` | `npm install -g copilot-api` | The Copilot → Anthropic proxy |
| Go (build only) | https://go.dev | To compile `qlaude` |

Authenticate the proxy **once**:

```sh
copilot-api auth
```

## Install

### One line (recommended)

```sh
curl -fsSL https://raw.githubusercontent.com/papesambandour/qlaude-code/main/install.sh | bash
```

This checks prerequisites, installs `copilot-api` (and `claude` if missing),
builds and installs the `qlaude` binary to `~/.local/bin`, and runs the
one-time `copilot-api auth` for you.

Override the install location or branch:

```sh
QLAUDE_PREFIX=/usr/local QLAUDE_REF=main \
  bash -c 'curl -fsSL https://raw.githubusercontent.com/papesambandour/qlaude-code/main/install.sh | bash'
```

### With Go

```sh
go install github.com/papesambandour/qlaude-code/cmd/qlaude@latest
```

### From source

```sh
git clone https://github.com/papesambandour/qlaude-code.git
cd qlaude-code
make install                 # -> ~/.local/bin/qlaude
# or a custom prefix:
make install PREFIX=/usr/local
```

Make sure the install dir (`~/.local/bin` by default) is on your `PATH`.

## Usage

```sh
qlaude                              # interactive Claude Code, via Copilot
qlaude -p "explain this repo"       # one-shot prompt (all claude flags work)
qlaude --resume                     # any native claude argument is forwarded
```

The very first run (or any run when the proxy is down) auto-starts `copilot-api`
and waits until it's ready before handing over to Claude Code. The proxy then
stays up as a background daemon for subsequent runs.

## Management commands

Management lives behind the reserved `--qlaude` prefix, so it can **never**
collide with a real Claude Code argument:

```sh
qlaude --qlaude status     # proxy status + selected models
qlaude --qlaude start      # start the copilot-api proxy
qlaude --qlaude stop       # stop it
qlaude --qlaude restart    # restart it
qlaude --qlaude logs [-f]  # show (or follow) proxy logs
qlaude --qlaude env        # print the env vars qlaude exports
qlaude --qlaude doctor     # diagnose the setup
qlaude --qlaude version    # qlaude version
qlaude --qlaude help       # help
```

## Models

`qlaude` auto-detects available models from the proxy's `/v1/models` endpoint,
so it always matches your Copilot plan. Defaults:

| Claude Code var | Meaning | Default |
|-----------------|---------|---------|
| `ANTHROPIC_MODEL` | Default model | `claude-opus-4.6` |
| `ANTHROPIC_DEFAULT_SONNET_MODEL` | Sonnet tier | newest `claude-sonnet-*` |
| `ANTHROPIC_DEFAULT_OPUS_MODEL` | Opus tier | `claude-opus-4.6` |
| `ANTHROPIC_DEFAULT_HAIKU_MODEL` / `ANTHROPIC_SMALL_FAST_MODEL` | Fast/background | newest `claude-haiku-*` |

Override any of them (see below). If a preferred model isn't in your plan,
`qlaude` gracefully falls back to the closest available one.

## Configuration (environment variables)

| Variable | Default | Description |
|----------|---------|-------------|
| `QLAUDE_PORT` | `4141` | Proxy port |
| `QLAUDE_HOST` | `127.0.0.1` | Proxy host |
| `QLAUDE_MODEL` | `claude-opus-4.6` | Override the default model |
| `QLAUDE_SONNET_MODEL` | auto | Override sonnet-tier model |
| `QLAUDE_OPUS_MODEL` | auto | Override opus-tier model |
| `QLAUDE_HAIKU_MODEL` | auto | Override haiku/fast model |
| `QLAUDE_NO_AUTOSTART` | `0` | Don't auto-start the proxy |
| `QLAUDE_START_TIMEOUT` | `45` | Seconds to wait for the proxy |
| `QLAUDE_KEEP_NONESSENTIAL` | `0` | Keep Claude Code non-essential traffic |
| `QLAUDE_COPILOT_API_CMD` | `copilot-api` | Proxy command |
| `QLAUDE_CLAUDE_CMD` | `claude` | Claude Code command |
| `QLAUDE_VERBOSE` | `0` | Verbose proxy + qlaude output |
| `QLAUDE_QUIET` | `0` | Silence qlaude's own messages |

Example:

```sh
QLAUDE_MODEL=claude-sonnet-5 qlaude -p "hello"
```

## How it works

```
qlaude ── ensures ──▶ copilot-api (localhost:4141)  ──▶  GitHub Copilot
   │                    (Anthropic-compatible /v1/messages)
   └── exec claude  with  ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN / ANTHROPIC_*MODEL
```

State (log + pid) lives in `~/.qlaude/`.

## Troubleshooting

- **`copilot-api is not authenticated`** → run `copilot-api auth` once.
- **Proxy won't start** → `qlaude --qlaude logs` to see why; `qlaude --qlaude doctor` for a checklist.
- **A "model retired" warning from Claude Code** → cosmetic client-side notice about the model name; requests still go through Copilot. Pick another model with `QLAUDE_MODEL=...` if you prefer.
- **Want to keep using the real Anthropic API** → just run `claude` directly instead of `qlaude`.
