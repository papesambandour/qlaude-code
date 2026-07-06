#!/usr/bin/env bash
#
# qlaude installer — sets up everything needed to run Claude Code through the
# GitHub Copilot proxy.
#
#   curl -fsSL https://raw.githubusercontent.com/papesambandour/qlaude-code/main/install.sh | bash
#
# It will:
#   1. check prerequisites (node, go, git)
#   2. install copilot-api (the Copilot -> Anthropic proxy) if missing
#   3. install Claude Code (`claude`) if missing (best effort)
#   4. build and install the `qlaude` binary
#   5. run `copilot-api auth` if you are not authenticated yet
#
# Environment overrides:
#   QLAUDE_PREFIX   install prefix (default: $HOME/.local)  -> binary in $PREFIX/bin
#   QLAUDE_REF      git branch/tag to build (default: main)
#   QLAUDE_NO_AUTH  set to 1 to skip the copilot-api auth step
#
set -euo pipefail

REPO_SLUG="papesambandour/qlaude-code"
REPO_URL="https://github.com/${REPO_SLUG}.git"
REF="${QLAUDE_REF:-main}"
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

# --- prerequisites -----------------------------------------------------------
info "Checking prerequisites"

have node || die "node is required. Install it: https://nodejs.org (or via nvm/asdf)"
ok "node    $(node -v)"

have npm  || die "npm is required (ships with Node.js)."
ok "npm     $(npm -v)"

have go   || die "Go is required to build qlaude. Install it: https://go.dev/dl"
ok "go      $(go version | awk '{print $3}')"

# --- copilot-api -------------------------------------------------------------
if have copilot-api; then
  ok "copilot-api already installed"
else
  info "Installing copilot-api (npm -g)"
  npm install -g copilot-api >/dev/null 2>&1 || die "failed to install copilot-api"
  ok "copilot-api installed"
fi

# --- claude (Claude Code) ----------------------------------------------------
if have claude; then
  ok "claude already installed"
else
  info "Installing Claude Code (best effort)"
  if npm install -g @anthropic-ai/claude-code >/dev/null 2>&1 && have claude; then
    ok "claude installed"
  else
    warn "could not auto-install claude. Install it manually:"
    warn "  npm install -g @anthropic-ai/claude-code"
    warn "  # or: curl -fsSL https://claude.ai/install.sh | bash"
    warn "qlaude will still install; claude is only needed at run time."
  fi
fi

# --- get the source ----------------------------------------------------------
SRC_DIR=""
CLEANUP=""
if [ -f go.mod ] && [ -d cmd/qlaude ] && grep -q "$REPO_SLUG" go.mod 2>/dev/null; then
  SRC_DIR="$(pwd)"
  info "Building from local checkout ($SRC_DIR)"
else
  have git || die "git is required to fetch the source."
  TMP_DIR="$(mktemp -d)"
  CLEANUP="$TMP_DIR"
  info "Cloning $REPO_URL@$REF"
  git clone --depth 1 --branch "$REF" "$REPO_URL" "$TMP_DIR" >/dev/null 2>&1 \
    || die "git clone failed (is the ref '$REF' correct?)"
  SRC_DIR="$TMP_DIR"
fi

# --- build -------------------------------------------------------------------
info "Building qlaude"
( cd "$SRC_DIR" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$SRC_DIR/qlaude" ./cmd/qlaude ) \
  || die "build failed"
ok "built qlaude"

# --- install -----------------------------------------------------------------
info "Installing to $BIN_DIR"
mkdir -p "$BIN_DIR"
install -m 0755 "$SRC_DIR/qlaude" "$BIN_DIR/qlaude"
ok "installed $BIN_DIR/qlaude"

[ -n "$CLEANUP" ] && rm -rf "$CLEANUP"

# --- PATH check --------------------------------------------------------------
case ":$PATH:" in
  *":$BIN_DIR:"*) : ;;
  *)
    warn "$BIN_DIR is not on your PATH. Add this to your shell rc:"
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
  copilot-api auth || warn "auth did not complete — run 'copilot-api auth' later"
else
  warn "copilot-api is not authenticated. Run once:  copilot-api auth"
fi

# --- done --------------------------------------------------------------------
echo
printf '%s\n' "${G}${B}Done!${N} qlaude is installed."
echo
echo "Try it:"
echo "  qlaude                     # interactive Claude Code via Copilot"
echo "  qlaude -p \"hello\"          # one-shot prompt (all claude flags work)"
echo "  qlaude --qlaude doctor     # verify the setup"
echo
