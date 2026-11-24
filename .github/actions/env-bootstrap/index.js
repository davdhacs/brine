// The runner injects ACTIONS_RESULTS_URL / ACTIONS_RUNTIME_TOKEN only
// into action (node) processes, not run steps — verified empirically
// fleet-wide 2025-11-24. This bootstrap forwards them via GITHUB_ENV so
// plain binaries (brine) in later run steps can speak the cache v2
// data plane. The runner masks the token in logs.
const fs = require('fs')

const vars = [
  'ACTIONS_RESULTS_URL',
  'ACTIONS_RUNTIME_TOKEN',
  'ACTIONS_CACHE_SERVICE_V2',
  'ACTIONS_CACHE_MODE',
]

const out = []
for (const v of vars) {
  if (process.env[v]) out.push(`${v}=${process.env[v]}`)
}
if (out.length) {
  fs.appendFileSync(process.env.GITHUB_ENV, out.join('\n') + '\n')
}
