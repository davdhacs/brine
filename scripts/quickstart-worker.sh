#!/bin/bash
# brine worker quickstart — paste this whole block into a shell on the
# Linux server. Builds brine from your clone, clones a public GitHub repo
# as the build target, configures Azure auth, starts the worker.
#
#   cd <your-brine-clone>   # then paste:
#   REPO_URL=https://github.com/prometheus/prometheus bash <(cat scripts/quickstart-worker.sh)
#
# Or standalone after git pull:
#   bash scripts/quickstart-worker.sh
#
# Variables (all optional):
#   REPO_URL    target repo to build (default: prometheus)
#   BUILD_CMD   the command clients will dispatch (default: build prometheus)
#   PORT        worker port (default 8081)
set -euo pipefail

say() { printf '\033[1;34m[brine-worker]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[brine-worker]\033[0m %s\n' "$*" >&2; exit 1; }

REPO_URL="${REPO_URL:-https://github.com/prometheus/prometheus}"
BUILD_CMD="${BUILD_CMD:-go build -o prometheus ./cmd/prometheus}"
PORT="${PORT:-8081}"
WORKER_HOME="${WORKER_HOME:-$HOME/brine-worker}"
ACCOUNT="${BRINE_AZURE_ACCOUNT:-cigocache}"
CONTAINER="${BRINE_GOCACHE_CONTAINER:-cimemo-test}"

# --- brine binary: build from this clone --------------------------------
[ -f cmd/brine/main.go ] || die "run from inside the brine clone (cd ~/brine-repo or wherever you cloned it)"
command -v go >/dev/null || die "Go not found — install it first (e.g. sudo apt install golang or from go.dev/dl)"
git pull --ff-only -q 2>/dev/null || true
mkdir -p "$WORKER_HOME/bin"
go build -o "$WORKER_HOME/bin/brine" ./cmd/brine
say "built brine: $WORKER_HOME/bin/brine"

# --- target repo ----------------------------------------------------------
REPO_NAME="$(basename "$REPO_URL" .git)"
if [ ! -d "$WORKER_HOME/repos/$REPO_NAME" ]; then
    mkdir -p "$WORKER_HOME/repos"
    git clone --depth 1 "$REPO_URL" "$WORKER_HOME/repos/$REPO_NAME"
    say "cloned $REPO_URL"
else
    say "reusing $WORKER_HOME/repos/$REPO_NAME"
fi

# --- Azure auth (prompted once, stored mode 600) ---------------------------
ENV_FILE="$WORKER_HOME/worker.env"
if [ ! -f "$ENV_FILE" ]; then
    say "Azure SPN credentials for the blob store (one-time; stored at $ENV_FILE)"
    read -rp "Azure tenant ID: " TENANT_ID
    read -rp "Azure client ID: " CID
    read -rsp "Azure client secret: " SECRET; echo
    [ -n "$TENANT_ID" ] && [ -n "$CID" ] && [ -n "$SECRET" ] || die "all three are required"
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

# --- worker launcher --------------------------------------------------------
cat > "$WORKER_HOME/start-worker.sh" <<EOF
#!/bin/bash
# brine worker — executes builds for $REPO_NAME on :$PORT
source "$ENV_FILE"
cd "$WORKER_HOME/repos/$REPO_NAME"
exec "$WORKER_HOME/bin/brine" worker --port $PORT
EOF
chmod +x "$WORKER_HOME/start-worker.sh"

# --- start + self-test ------------------------------------------------------
say "starting worker on :$PORT"
nohup "$WORKER_HOME/start-worker.sh" > "$WORKER_HOME/worker.log" 2>&1 &
echo $! > "$WORKER_HOME/worker.pid"
sleep 1
curl -fsS "http://localhost:$PORT/health" >/dev/null || { tail -5 "$WORKER_HOME/worker.log"; die "worker failed to start"; }
say "worker healthy (pid $(cat "$WORKER_HOME/worker.pid"), log: $WORKER_HOME/worker.log)"

# --- client instructions ------------------------------------------------------
SERVER_IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
say "done. from your client machine, dispatch a build:"
say "  brine remote-build --worker http://${SERVER_IP:-<server-ip>}:$PORT -- $BUILD_CMD"
say "first dispatch cold-builds (module downloads + compile); second hits the receipt."
