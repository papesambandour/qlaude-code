#!/usr/bin/env bash
#
# qlaude installer — sets up everything needed to run Claude Code through the
# GitHub Copilot proxy.
#
#   curl -fsSL https://raw.githubusercontent.com/papesambandour/qlaude-code/main/install.sh | bash
#
# What it does:
#   1. Detects your OS and CPU architecture
#   2. Downloads the pre-built qlaude binary for your platform (no Go needed)
#   3. Installs copilot-api (the Copilot -> Anthropic proxy) if missing
#   4. Installs Claude Code (`claude`) if missing (best effort)
#   5. Runs `copilot-api auth` if you are not authenticated yet
#
# Fallback: if no pre-built binary matches your platform, builds from source
# using `go` (which must be installed in that case).
#
# Environment overrides:
#   QLAUDE_PREFIX   install prefix (default: $HOME/.local)  -> binary in $PREFIX/bin
#   QLAUDE_VERSION  release tag to download (default: latest)
#   QLAUDE_NO_AUTH  set to 1 to skip the copilot-api auth step
#
set -euo pipefail

REPO="papesambandour/qlaude-code"
REPO_URL="https://github.com/${REPO}.git"
PREFIX="${QLAUDE_PREFIX:-$HOME/.local}"
BIN_DIR="$PREFIX/bin"

# --- pretty output -----------------------------------------------------------
if [ -t 1 ]; then
  B=$(printf '\033[1m'); G=$(printf '\033[32m'); Y=$(printf '\033[33m')
  R=$(printf '\033[31m'); C=$(printf '\033[36m'); N=$(printf '\033[0m')
else
  B=""; G=""; Y=""; R=""; C=""; N=""
fi
info()  { printf '%s\n' "${C}==>${N} $*"; }
ok()    { printf '%s\n' "${G} ok${N}  $*"; }
warn()  { printf '%s\n' "${Y}warn${N}  $*"; }
die()   { printf '%s\n' "${R}fail${N}  $*" >&2; exit 1; }
have()  { command -v "$1" >/dev/null 2>&1; }

printf '%s\n' "${B}qlaude installer${N} — Claude Code powered by GitHub Copilot"
echo

# --- detect OS + arch --------------------------------------------------------
info "Detecting platform"

PLATFORM_OS=""
case "$(uname -s)" in
  Darwin)             PLATFORM_OS="darwin" ;;
  Linux)              PLATFORM_OS="linux" ;;
  MINGW*|MSYS*|CYGWIN*) PLATFORM_OS="windows" ;;
  *) warn "Unsupported OS: $(uname -s) — will try to build from source" ;;
esac

PLATFORM_ARCH=""
case "$(uname -m)" in
  x86_64|amd64)    PLATFORM_ARCH="amd64" ;;
  arm64|aarch64)   PLATFORM_ARCH="arm64" ;;
  *) warn "Unsupported arch: $(uname -m) — will try to build from source" ;;
esac

[ -n "$PLATFORM_OS" ]   && ok "OS    $PLATFORM_OS"
[ -n "$PLATFORM_ARCH" ] && ok "arch  $PLATFORM_ARCH"

# --- resolve latest release version -----------------------------------------
QLAUDE_VERSION="${QLAUDE_VERSION:-}"
if [ -z "$QLAUDE_VERSION" ]; then
  QLAUDE_VERSION=$(curl -fsSL \
    "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null \
    | python3 -c 'import sys,json;print(json.load(sys.stdin).get("tag_name",""))' 2>/dev/null \
    || echo "")
fi
if [ -z "$QLAUDE_VERSION" ]; then
  QLAUDE_VERSION="v0.1.1"
  warn "Could not detect latest release version, defaulting to $QLAUDE_VERSION"
fi
ok "version  $QLAUDE_VERSION"

# --- download pre-built binary -----------------------------------------------
DOWNLOADED=0
if [ -n "$PLATFORM_OS" ] && [ -n "$PLATFORM_ARCH" ]; then
  BIN_NAME="qlaude_${PLATFORM_OS}_${PLATFORM_ARCH}"
  [ "$PLATFORM_OS" = "windows" ] && BIN_NAME="${BIN_NAME}.exe"
  DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${QLAUDE_VERSION}/${BIN_NAME}"

  info "Downloading ${BIN_NAME} from ${QLAUDE_VERSION}"
  TMP_BIN="$(mktemp)"
  HTTP_CODE=$(curl -fsSL -o "$TMP_BIN" -w "%{http_code}" "$DOWNLOAD_URL" 2>/dev/null || echo "000")
  if [ "$HTTP_CODE" = "200" ] && [ -s "$TMP_BIN" ]; then
    chmod +x "$TMP_BIN"
    mkdir -p "$BIN_DIR"
    install -m 0755 "$TMP_BIN" "$BIN_DIR/qlaude"
    rm -f "$TMP_BIN"
    ok "downloaded and installed $BIN_DIR/qlaude"
    DOWNLOADED=1
  else
    rm -f "$TMP_BIN"
    warn "Pre-built binary not available (http $HTTP_CODE) — will try building from source"
  fi
fi

# --- fallback: build from source ---------------------------------------------
if [ "$DOWNLOADED" = "0" ]; then
  info "Falling back to build from source"
  have go || die "No pre-built binary for your platform AND 'go' is not in PATH.\nInstall Go: https://go.dev/dl  or set QLAUDE_PREFIX and QLAUDE_VERSION."
  have git || die "git is required to clone the source."

  ok "go      $(go version | awk '{print $3}')"

  SRC_DIR=""
  # Reuse local checkout if already inside the repo
  if [ -f go.mod ] && [ -d cmd/qlaude ] && grep -q "$REPO" go.mod 2>/dev/null; then
    SRC_DIR="$(pwd)"
    info "Building from local checkout ($SRC_DIR)"
  else
    TMP_SRC="$(mktemp -d)"
    info "Cloning ${REPO_URL}@${QLAUDE_VERSION#v}"
    git clone --depth 1 --branch "${QLAUDE_VERSION}" "$REPO_URL" "$TMP_SRC" >/dev/null 2>&1 \
      || git clone --depth 1 "$REPO_URL" "$TMP_SRC" >/dev/null 2>&1 \
      || die "git clone failed"
    SRC_DIR="$TMP_SRC"
  fi

  info "Building qlaude"
  REF="${QLAUDE_VERSION#v}"
  ( cd "$SRC_DIR" && CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${REF}" \
      -o "$SRC_DIR/qlaude_src" ./cmd/qlaude ) || die "build failed"

  mkdir -p "$BIN_DIR"
  install -m 0755 "$SRC_DIR/qlaude_src" "$BIN_DIR/qlaude"
  rm -f "$SRC_DIR/qlaude_src"
  [ -n "${TMP_SRC:-}" ] && rm -rf "${TMP_SRC:-}"
  ok "built and installed $BIN_DIR/qlaude"
fi

# --- prerequisites: copilot-api ----------------------------------------------
info "Checking dependencies"

have node || die "node is required. Install it: https://nodejs.org"
ok "node    $(node -v)"
have npm  || die "npm is required (ships with Node.js)."
ok "npm     $(npm -v)"

if have copilot-api; then
  ok "copilot-api already installed"
else
  info "Installing copilot-api (npm -g)"
  npm install -g copilot-api >/dev/null 2>&1 || die "failed to install copilot-api"
  ok "copilot-api installed"
fi

# --- prerequisites: claude (Claude Code) -------------------------------------
if have claude; then
  ok "claude already installed"
else
  info "Installing Claude Code (best effort)"
  if npm install -g @anthropic-ai/claude-code >/dev/null 2>&1 && have claude; then
    ok "claude installed"
  else
    warn "Could not auto-install claude. Install it manually:"
    warn "  npm install -g @anthropic-ai/claude-code"
    warn "qlaude is installed and will work once claude is available."
  fi
fi

# --- PATH check --------------------------------------------------------------
case ":$PATH:" in
  *":$BIN_DIR:"*) : ;;
  *)
    warn "$BIN_DIR is not on your PATH. Add this line to your shell rc:"
    printf '      %s\n' "export PATH=\"$BIN_DIR:\$PATH\""
    ;;
esac

# --- copilot-api auth --------------------------------------------------------
TOKEN_FILE="$HOME/.local/share/copilot-api/github_token"
if [ "${QLAUDE_NO_AUTH:-0}" = "1" ]; then
  info "Skipping copilot-api auth (QLAUDE_NO_AUTH=1)"
elif [ -f "$TOKEN_FILE" ]; then
  ok "copilot-api already authenticated"
elif [ -t 0 ]; then
  info "Authenticating copilot-api (one-time GitHub device login)"
  copilot-api auth || warn "Auth did not complete — run 'copilot-api auth' to finish."
else
  warn "copilot-api is not authenticated. Run once:  copilot-api auth"
fi

# --- done --------------------------------------------------------------------
echo
printf '%s\n' "${G}${B}Done!${N} qlaude ${QLAUDE_VERSION} is installed."
echo
echo "Try it:"
echo "  qlaude                     # interactive Claude Code via Copilot"
echo "  qlaude -p \"hello\"          # one-shot prompt (all claude flags work)"
echo "  qlaude --qlaude doctor     # verify the setup"
echo

