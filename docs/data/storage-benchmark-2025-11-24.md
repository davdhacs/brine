# storage-benchmark results (E10b) — 2025-11-24

12 runs, 6 runner CPU models (Xeon Platinum 8573C ×3, Xeon 6975P-C ×2,
EPYC 7763 ×3, EPYC 9V45 ×2, EPYC 9V74 ×2, Xeon Platinum 8370C ×1),
reps=3 per cell, medians over ~30 samples per (path,size).

## Throughput (100 MB payloads)

| path | op | median MB/s | per-CPU range |
|---|---|---|---|
| cache v2 (SAS blob PUT) | upload | 44 | 34–51 |
| cache v2 (SAS blob GET) | download, single | 70 | 29–133 |
| cache v2 (SAS blob GET) | download, 4× ranged | 124 | — |
| Azure westus3 (ours) | upload | 35 | — |
| Azure westus3 (ours) | download, single | 38 | — |
| Azure westus3 (ours) | download, 4× ranged | 73 | — |
| artifact v4 (official client) | upload | 25 | n=10 |
| artifact v4 (official client) | download | ~105 | n=1 + earlier batch |

Smaller payloads are latency-dominated: 1MB ops ≈ 226–530ms.

## Findings

1. Cache v2 SAS blobs are same-region (productionresultssa{2,4,6,8,9,16,18}
   .blob.core.windows.net) — the fastest path from runners: 70 MB/s
   single-stream, 124 MB/s with 4-way ranged GET (range works: 206).
2. Our Azure westus3 is cross-region from runners: 35-38 MB/s single,
   73 MB/s ranged. Competitive but slower; fine as the cross-machine
   backend, not the CI-fastest.
3. Per-CPU-model variance is LARGE and real: the same cache download
   spans 29 MB/s (Xeon 8370C) to 133 MB/s (EPYC 7763) — runner fleet
   network, with CPU model as the observable proxy. Justifies the
   calibration-probe + cpuinfo-recording methodology (E9b).
4. Artifact v4 via the official client is the slowest upload path
   (25 MB/s) and was flaky in-job (11/12 download steps failed with
   "artifact not found" shortly after upload — eventual-consistency
   or fleet issue; observed, not diagnosed).
5. Upload path for cache v2 is direct-to-blob via write SAS
   (CreateCacheEntry → signed_upload_url → PUT → Finalize).

## Protocol verification (E10 checklist — all 12 runs agree)

- v1 (ACTIONS_CACHE_URL) absent fleet-wide; v2 twirp via
  ACTIONS_RESULTS_URL (results-receiver.actions.githubusercontent.com)
- CreateCacheEntry{key, version=sha256-hex} → {ok, signed_upload_url};
  version must be sha256-hex-like (a 13-char string was rejected:
  "version invalid length ... must be between 1 and 64 characters")
- duplicate key → ok=false, msg "cache entry with the same key,
  version, and scope already exists" (= PutIfNotExist)
- GetCacheEntryDownloadURL → {ok, signed_download_url, matched_key}
- ranged GET on the SAS URL: 206 ✓
- runtime token/URL not in run-step envs — env-bootstrap action
  (GITHUB_ENV forwarding) works
