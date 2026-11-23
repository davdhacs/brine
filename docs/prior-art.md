# Prior art (verified sources, 2025-11-23)

Literature survey for the paper. Status: URL-verified unless noted.
Positioning note at the end.

## Theory / frameworks

1. **Build Systems à la Carte** — Mokhov, Mitchell, Peyton Jones.
   ICFP 2018 / ACM PACMPL. DOI 10.1145/3236774.
   THE theory framework: build systems as monotone functions, three
   axes (scheduler, rebuilder, verifier). Brine's receipts = the
   "verifying" axis realized at command granularity, composable with
   any scheduler (make, cmake, go build).
2. **Recursive Make Considered Harmful** — Peter Miller, AUUGN 1998.
   (well-known classic; verify exact citation at writing time)

## Automatic dependency detection (brine's observation lineage)

3. **Tup** — Mike Shal, 2009. gittup.org/tup
   - "Build System Rules and Algorithms":
     gittup.org/tup/build_system_rules_and_algorithms.pdf
   - Chad Austin's analysis: chadaustin.me/2010/06/scalable-build-systems-an-analysis-of-tup/
   File-based build system, automatic dep detection via syscall
   monitoring (inotify/fanotify, LD_PRELOAD). Direct ancestor of
   brine's observation sensor. Difference: Tup requires Tupfiles and
   its own build flow; brine wraps arbitrary existing commands.
4. **Fabricate** — Python build tool, ~2010.
   github.com/brushtechnology/fabricate
   strace-based automatic dependency discovery. The observation idea
   in its simplest form. Notes strace's limitations (misses deps,
   platform-specific).

## Content-addressed build caching

5. **Vesta** — DEC/Compaq SRC Research Report 177, "The Vesta
   Approach to Software Configuration Management" (2002).
   vesta.sourceforge.net/doc/pubs/src-rr-177/
   Content-addressed derived-object caching, repeatable builds for
   10M+ LOC codebases. The oldest ancestor of the CA build cache.
   Required adopting Vesta's model wholesale; brine memoizes existing
   commands without adoption.
6. **Nix** — Dolstra, "The Purely Functional Software Deployment
   Model", PhD thesis 2006. edolstra.github.io/pubs/phd-thesis.pdf
   Store paths derived from hashes of build inputs. Declarative
   derivations vs brine's observation-derived keys.

## Distributed compilation / remote execution

7. **distcc** — Martin Pool, linux.conf.au 2004.
   distcc.org/distcc-lca-2004.pdf
   Distributes compilation only; brine memoizes whole commands
   including links, with output verification.
8. **Goma** — Google distributed compile service (Chromium/Android).
   chromium.googlesource.com/infra/goma/server/
   Uses REAPI. Backend historically Google-internal.
9. **Bazel/Blaze** — NO formal paper exists for Blaze. Citable:
   - arXiv:2405.00796 — Bazel CI performance study
   - Potvin & Levesque, "Why Google Stores Billions of Lines of Code
     in a Single Repository", CACM 2016 (verify at writing time)
   - Bazel remote cache / REAPI docs
   Blaze started ~2006 internally, Bazel released 2015. Artifact-based
   builds; requires BUILD files (the declaration barrier brine avoids).
10. **Buck2** — Meta, Rust + Starlark. buck2.build — docs only,
    no formal paper found.

## Reproducibility / certainty

11. **Reproducible Builds** — Lamb & Zacchiroli, IEEE Software 2021.
    arXiv:2104.06020 / DOI 10.1109/MS.2021.3073085 (verify DOI).
    "Reproducible Builds: Increasing the Integrity of Software Supply
    Chains." Connects to the parity gate and verify-on-restore.

## Incremental builds (functional)

12. **Shake** — Mitchell, "Shake Before Building: Replacing Make with
    Haskell", ICFP 2012. ndmitchell.com/downloads/paper-shake_before_building-10_sep_2012.pdf
    Dynamic dependencies specified during the build.

## Searched, NOT found (do not cite without further digging)

- "Maelstrom" Google remote execution — product/blog only, no paper.
- "TACOS" Meta CI caching — likely misremembered name; no paper found.
- Blaze formal paper — does not appear to exist.
- Buck2 formal paper — does not appear to exist.

## Positioning (one paragraph, paper draft)

Build systems research divides along whether dependency knowledge is
declared (Make, Bazel, Buck2, Shake, Nix) or observed (Tup, Fabricate).
Content-addressed caching of build outputs dates to Vesta (1992-2002)
and reaches its purest form in Nix; Bazel brought it to CI at scale
behind a BUILD-file declaration barrier. Brine occupies the unpopulated
corner: observation-derived, content-addressed memoization of existing
commands — no build-language adoption, no BUILD files — with
Bazel-composable serving (remote cache protocol) and remote execution.
Tup proved syscall-level observation yields exact dependencies but
bound it to its own build system; brine unbinds it.
