# remote-build: dispatch compute, restore verified outputs

`brine worker` and `brine remote-build` implement remote build execution
over the shared blob store — Bazel's remote-execution benefit without
BUILD files or a REAPI cluster.

```
# machine A (worker, repo checked out):
BRINE_REMOTE=1 brine worker --port 8081

# machine B (client, same repo layout):
BRINE_REMOTE=1 brine remote-build --worker http://a:8081 -- go build -o kwtest ./cmd/kwtest
```

## Flow

1. Client POSTs the command to the worker.
2. Worker checks out the requested commit (optional), runs the command
   under brine — receipts, output blobs, and gocache entries upload to
   the shared blob store — and reports the receipt key via
   `BRINE_KEY_OUT` (machine-readable key channel added to the wrap flow).
3. Client fetches the manifest by key (local store, then remote),
   restores output blobs with verify-on-restore (sha256 == key, enforced
   in GetBlob), and places them at the manifest's relative paths.

The client never runs the toolchain. Content addressing means the
client's trust boundary is the hash: a malicious or corrupted worker
cannot serve wrong bytes without the client detecting it on restore.

## Measured (localhost two-clone demo, Azure westus3 blob store)

| phase | wall |
|---|---|
| worker cold compile | 2.2s |
| worker receipt hit | 145ms |
| client restore (11MB binary, verified) | 2.4s |

The client restore is dominated by blob transfer — same-region Azure CI
would be substantially faster, and the serve batch endpoint (planned)
would amortize per-blob request overhead for multi-output builds.

## Notes

- `BRINE_REMOTE=1` must be set on both sides; the client's remote gate
  sets `BRINE_GOCACHE_REMOTE=1` internally (same convention as wrap).
- Workers are stateless apart from their repo checkout and local cache
  dirs; any machine with the sources can be a worker.
- Token auth (`--token` / `BRINE_SERVE_TOKEN`) mirrors serve.
- The worker reports the receipt key from every wrap exit path: commit
  fast-path hit, content-key hit, keyed hit, and fresh store.
