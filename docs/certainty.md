# Certainty model

## Guaranteed (by construction)

- remote-sourced blobs: sha256(content) == key on arrival; corrupt or
  poisoned blobs degrade to misses, never serve wrong bytes
- ALL blobs re-verified on restore (local ones too; opt-out
  `CIMEMO_TRUST_LOCAL=1` skips ~1s/GB)
- keyed receipts hash every input file (no mtime trust)
- commit receipts are git-certain (clean tree at SHA ⟹ exact contents)
- docker image receipts fold the registry-current base digest

## Measured (CI gate, every run)

- GROUND TRUTH leg: the receipt-restored binary is byte-compared against
  a PLAIN no-cimemo build of the same tree
- buildinfo Verify on restore (vcs.revision, ldflags)

## Advisory (audit layers)

- buildinfo audit: VCS stamps, external ldflags
- pin audit (`pinaudit`): floating dependency references
- divergence detector: same closure, different output → binary dissection

## Known residuals

- non-go toolchain env in receipt keys (gcc version, CGO_CFLAGS) — go's
  action IDs catch it for the action cache; receipts do not
- local warm path trusts mtimes (superseded by hash-certain keyed path
  on fresh machines; still reachable locally)
