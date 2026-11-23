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
3. Client fetches the manifest by key (local store, then remote) and
   reports what's available. Output blobs stay in the shared store
   until a consumer needs them: `brine restore <key> [pattern]`
   fetches selectively with verify-on-restore; `--restore` fetches
   everything eagerly.

This is the Bazel REAPI model — action result separated from CAS blob
fetching. The default dispatch downloads only the manifest (~KBs of
metadata); a CI gate that just needs "did it build?" transfers nothing
else, and the consumer that needs the binary pulls exactly that blob.

```
brine remote-build --worker http://a:8081 -- make -j8
# ✓ completed (receipt 494f54a632ce6410)
#   898 outputs, 1619.3 MB in shared store
#   cmake-build/collector/collector (81.7 MB)
#   ...
#   fetch with: brine restore 494f54a632ce6410 [pattern]

brine restore 494f54a632ce6410 cmake-build/collector/collector   # substring
brine restore 494f54a632ce6410 "*.o"                             # glob
brine restore 494f54a632ce6410                                   # everything
```

Pattern semantics: substring match on the manifest-relative path, or
`filepath.Match` glob when the pattern contains `*`, `?`, or `[`.

The client never runs the toolchain. Content addressing means the
client's trust boundary is the hash: a malicious or corrupted worker
cannot serve wrong bytes without the client detecting it on restore.

## Measured

localhost two-clone demo (k8swatch, Azure westus3):

| phase | wall |
|---|---|
| worker cold compile | 2.2s |
| worker receipt hit | 145ms |
| client restore (11MB binary, verified) | 2.4s |

real two-machine run (mac client → Linux homelab worker "tone",
prometheus, 148MB binary, same Azure store):

| phase | wall |
|---|---|
| worker cold build (module downloads + compile) | 197s |
| worker warm relink (gocache hit) | 15.2s |
| worker receipt hit | 1.9s |
| client restore (148MB streamed from Azure, sha256 verified) | 33s |

collector (mac client → tone worker, cmake build inside the
collector-builder Docker container, 898 outputs / 1.6GB):

| phase | wall |
|---|---|
| worker remote-build, eager restore (old default) | 21m9s total |
| selective restore of just the 82MB binary | 18.9s |

The eager leg was dominated by two costs the report-only model
removes: 1.6GB of blob transfer nobody asked for, and (on the worker
side) a 20K-file git-tracked closure — docker exec is opaque to
observation. Docker-exec commands now fold the container's image
digest, mount table, and env into the receipt key (see
`docs/config.md`, key_images), and the closure-precision fix is
tracked in the roadmap.

The client restore is dominated by blob transfer from the Azure region —
same-region CI would be substantially faster. Blob fetches stream to
disk with stall detection (no total timeout): a fixed 30s cap killed
legitimate 148MB transfers and the error was masked as "blob not found"
until the streaming fix. Hit paths now re-share local receipts to the
remote store (EnsureRemote, existence-checked) — receipts built while
the remote was down no longer stay invisible to other machines.

## Notes

- `BRINE_REMOTE=1` must be set on both sides; the client's remote gate
  sets `BRINE_GOCACHE_REMOTE=1` internally (same convention as wrap).
- Workers are stateless apart from their repo checkout and local cache
  dirs; any machine with the sources can be a worker.
- Token auth (`--token` / `BRINE_SERVE_TOKEN`) mirrors serve.
- The worker reports the receipt key from every wrap exit path: commit
  fast-path hit, content-key hit, keyed hit, and fresh store.
