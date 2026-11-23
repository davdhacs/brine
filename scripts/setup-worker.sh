#!/bin/bash
# brine worker setup — copy/paste on a Linux server.
#
# Turns the server into a brine build worker: executes build commands on
# request, uploads receipts + output blobs to the shared Azure blob store.
# Clients (your laptop) dispatch builds and restore verified outputs.
#
# Prereqs:
#   - the brine binary at ~/brine (see step 0 on the client side), or Go
#     1.22+ to build it from source
#   - Go on the server if you'll dispatch go builds (any recent version)
#   - Azure SPN credentials for the blob store (tenant/client/secret)
#
# Usage:
#   bash setup-worker.sh            # interactive (prompts once, saves env)
#   bash setup-worker.sh --check    # verify an existing setup
set -euo pipefail

BRINE_HOME="${BRINE_HOME:-$HOME/brine-worker}"
PORT="${PORT:-8081}"
ACCOUNT="${BRINE_AZURE_ACCOUNT:-cigocache}"
CONTAINER="${BRINE_GOCACHE_CONTAINER:-cimemo-test}"
# SPN with Storage Blob Data Contributor on the account (group
# acs-ci-gocache-users membership also works via user tokens, but SPN
# credentials don't expire hourly — the right choice for a server).
CLIENT_ID="${AZURE_CLIENT_ID:-169682eb-8026-4cb2-a2ef-1905b6815811}"

say() { printf '\033[1;34m[worker-setup]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[worker-setup]\033[0m %s\n' "$*" >&2; exit 1; }

mkdir -p "$BRINE_HOME"/{bin,repo}

# ---------------------------------------------------------------- binary
find_brine() {
    if [ -x "$HOME/brine" ]; then echo "$HOME/brine"; return; fi
    if [ -x "$BRINE_HOME/bin/brine" ]; then echo "$BRINE_HOME/bin/brine"; return; fi
    if command -v brine >/dev/null; then command -v brine; return; fi
    echo ""
}

BRINE_BIN="$(find_brine)"
if [ -z "$BRINE_BIN" ]; then
    say "no brine binary found — building from source (needs Go)"
    command -v go >/dev/null || die "install Go or scp a brine binary to ~/brine
  (from the mac: scp /Volumes/dev/stack/brine/bin/brine-linux-amd64 you@server:~/brine   # x86_64 server
                   scp /Volumes/dev/stack/brine/bin/brine-linux-arm64 you@server:~/brine   # aarch64 server)"
    git clone -q https://github.com/davdhacs/brine "$BRINE_HOME/src" 2>/dev/null || true
    [ -d "$BRINE_HOME/src" ] || die "clone failed"
    (cd "$BRINE_HOME/src" && go build -o "$BRINE_HOME/bin/brine" ./cmd/brine)
    BRINE_BIN="$BRINE_HOME/bin/brine"
    say "built $BRINE_BIN"
fi
say "brine: $BRINE_BIN"

# ---------------------------------------------------------------- env/auth
ENV_FILE="$BRINE_HOME/worker.env"
if [ "${1:-}" = "--check" ]; then
    [ -f "$ENV_FILE" ] || die "no $ENV_FILE — run the script without --check first"
    source "$ENV_FILE"
    BRINE_BIN="$(find_brine)"; [ -n "$BRINE_BIN" ] || die "brine binary missing"
    say "checking worker at :$PORT"
    curl -fsS "http://localhost:$PORT/health" && say "healthy" || die "not running"
    exit 0
fi

if [ ! -f "$ENV_FILE" ]; then
    say "Azure SPN auth (stored in $ENV_FILE, mode 600)"
    read -rp "Azure tenant ID: " TENANT_ID
    read -rp "Azure client ID [$CLIENT_ID]: " CID; CID="${CID:-$CLIENT_ID}"
    read -rsp "Azure client secret: " SECRET; echo
    umask 077
    cat > "$ENV_FILE" <<EOF
# brine worker environment — generated $(date -Is)
export BRINE_REMOTE=1
export BRINE_GOCACHE_REMOTE=1
export BRINE_AZURE_ACCOUNT=$ACCOUNT
export BRINE_GOCACHE_CONTAINER=$CONTAINER
export AZURE_TENANT_ID=$TENANT_ID
export AZURE_CLIENT_ID=$CID
export AZURE_CLIENT_SECRET=$SECRET
EOF
    say "wrote $ENV_FILE"
else
    say "reusing $ENV_FILE"
fi

# ---------------------------------------------------------------- repo
if [ ! -d "$BRINE_HOME/repo/.git" ] && [ ! -f "$BRINE_HOME/repo/go.mod" ]; then
    say "no repo at $BRINE_HOME/repo — rsync it from the client:"
    say "  rsync -a --exclude .git --exclude .brine --exclude bazel-* --exclude .cimemo <client-repo>/ $USER@$(hostname):$BRINE_HOME/repo/"
    say "(or clone your project there yourself; the worker runs in that dir)"
fi

command -v go >/dev/null && say "go: $(go version)" || say "NOTE: no Go on server — go builds will fail, non-go commands still work"

# ---------------------------------------------------------------- start
cat > "$BRINE_HOME/start-worker.sh" <<EOF
#!/bin/bash
# brine worker — serves build execution on :$PORT
source "$ENV_FILE"
cd "$BRINE_HOME/repo"
exec "$BRINE_BIN" worker --port $PORT
EOF
chmod +x "$BRINE_HOME/start-worker.sh"

say "setup complete. start the worker:"
say "  $BRINE_HOME/start-worker.sh            # foreground"
say "  nohup $BRINE_HOME/start-worker.sh >/tmp/brine-worker.log 2>&1 &   # background"

if [ "${NO_START:-0}" != "1" ]; then
    say "starting worker now (ctrl-C stops; use start-worker.sh to restart)"
    "$BRINE_HOME/start-worker.sh"
fi
