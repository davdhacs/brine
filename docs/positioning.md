# Positioning: cimemo vs Bazel

Bazel/Buck2 solve the key-closure problem by CONSTRUCTION: explicit
declared dependencies, hermetic actions, content-addressed remote
caching and execution. cimemo retrofits memoization onto whatever
build system you already have.

| | Bazel | cimemo |
|---|---|---|
| migration | rewrite the build | none — wrap the command |
| soundness | by construction | discovered (sensors + audits + gates) |
| caching granularity | actions | whole outputs (receipts) + protocol tiers |
| remote execution | yes | no |
| build systems | the ones you port to | go, cargo, npm, make, cmake, docker, ... |

They compose: cimemo receipts above `bazel build` (memoize analysis +
setup); Bazel's remote cache within. cimemo's GOCACHEPROG/ccache/sccache
tiers are the no-migration equivalent of Bazel's remote cache.

Where Bazel wins: hermetic reproducibility, remote execution, polyglot
graph queries. Where cimemo wins: zero adoption cost, works today,
receipt-level caching (skip the tool entirely).
