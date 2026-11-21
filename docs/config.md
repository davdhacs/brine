# Configuration tiers

Precedence: env var > `.cimemo.json` > defaults.

## Tier 0 — default (no config, no env)

Local store, all sensors on, full verification. Safe on anything.

## Tier 1 — project (`.cimemo.json`, checked in)

```json
{
  "key_env": ["FEATURE", "CFLAGS"],
  "key_files": ["flags.conf"],
  "buildvcs": "key-fold"
}
```

Discovered by `cimemo record <command>` — runs instrumented, finds
hidden inputs, writes the config. See `cmd/cimemo/record.go`.

## Tier 2 — CI (env overrides)

```
CIMEMO_REMOTE=1
CIMEMO_AZURE_ACCOUNT=<account>
CIMEMO_GOCACHE_CONTAINER=<container>
CIMEMO_TRUST_LOCAL=1        # only with a byte-identity CI gate
```

Every knob independently settable. See `cmd/cimemo/config.go` for the
full schema.
