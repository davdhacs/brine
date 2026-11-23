#!/bin/sh
# Collector remote-build with output_patterns: cold (relink) then warm.
set -e
BRINE=/Volumes/dev/stack/brine/bin/brine
cd /Volumes/dev/stack/brine/.collector-rb
az account get-access-token --resource https://storage.azure.com --query accessToken -o tsv > /tmp/cimemo-az-token
export BRINE_REMOTE=1 BRINE_GOCACHE_REMOTE=1 BRINE_AZURE_ACCOUNT=cigocache BRINE_GOCACHE_CONTAINER=cimemo-ci
export BRINE_AZURE_TOKEN="$(cat /tmp/cimemo-az-token)"
CMD="docker exec collector-builder cmake --build /tmp/collector/cmake-build -- -j4"

# remove the binaries so cmake actually relinks (objects remain)
PREP='rm -f ~/brine-worker/repos/collector/cmake-build/collector/collector ~/brine-worker/repos/collector/cmake-build/collector/self-checks'
B64=$(printf '%s' "$PREP" | base64 | tr -d '\n')
curl -s --max-time 30 -X POST http://192.168.1.135:8081/execute -H 'Content-Type: application/json' \
  -d "{\"args\":[\"sh\",\"-c\",\"echo $B64 | base64 -d | sh; exit 1\"]}" > /dev/null
echo "binaries removed"

echo "=== COLD dispatch (relink + scoped snapshot + 85MB upload) ==="
date +%s > .t0
$BRINE remote-build --worker http://192.168.1.135:8082 -- $CMD 2>&1
echo "=== WARM dispatch (receipt hit) ==="
$BRINE remote-build --worker http://192.168.1.135:8082 -- $CMD 2>&1
