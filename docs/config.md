# Configuration tiers

Precedence: env var > `.brine.json` > defaults.

## Tier 0 — default (no config, no env)

Local store, all sensors on, full verification. Safe on anything.

## Tier 1 — project (`.brine.json`, checked in)

```json
{
  "key_env": ["FEATURE", "CFLAGS"],
  "key_files": ["flags.conf"],
  "key_images": ["quay.io/stackrox-io/collector-builder:master"],
  "buildvcs": "key-fold"
}
```

Discovered by `brine record <command>` — runs instrumented, finds
hidden inputs, writes the config. See `cmd/brine/record.go`.

## Docker exec: automatic environment keying

Commands of the form `docker exec <container> ...` fold the
container's identity into the receipt key automatically — no config
needed: the image digest (a content address for the entire toolchain:
compilers, headers, sysroot — one hash replaces hashing the image
contents), the mount table (host↔container mapping; host-side source
paths are dropped so receipts share across workers with the repo at
different paths), and the run-time container env (sorted). Without
this, a builder container recreated from a different image served
stale receipts — the image was an invisible input.

`key_images` (above) is the manual form for other shapes: `docker
run`-style commands where the image comes from the command line, or
images that influence the build without appearing in it (a sidecar
the build talks to). Each ref resolves to its local digest at key
time.

## Tier 2 — CI (env overrides)

```
CIMEMO_REMOTE=1
CIMEMO_AZURE_ACCOUNT=<account>
CIMEMO_GOCACHE_CONTAINER=<container>
CIMEMO_TRUST_LOCAL=1        # only with a byte-identity CI gate
```

Every knob independently settable. See `cmd/brine/config.go` for the
full schema.
