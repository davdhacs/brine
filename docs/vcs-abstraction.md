# VCS abstraction boundary

cimemo uses git for five specific capabilities, not as a monolithic
dependency. This document maps the coupling points and what a non-git
VCS would need to provide.

## The five capabilities

| # | Capability | Git invocation | Used by |
|---|---|---|---|
| 1 | **List tracked files** | `git ls-files -s` | read-set fallback, pin audit |
| 2 | **Current revision ID** | `git rev-parse HEAD` | VCS key-folding, commit receipts |
| 3 | **Tree cleanliness** | `git diff-index --quiet HEAD` | commit receipt validity, record mode |
| 4 | **Submodule/gitlink resolution** | `git -C <sub> rev-parse HEAD` | dependency tracking for submodules |
| 5 | **File-is-tracked check** | `git ls-files -- <file>` | untracked-lockfile detection (pin audit) |

Plus one file-format dependency: `.gitignore` parsing (record mode's
ignored-config discovery, docker context closure exclusion).

## What a non-git VCS needs

| Capability | SVN | Mercurial | Perforce | Fossil |
|---|---|---|---|---|
| List tracked files | `svn list -R` | `hg files` | `p4 have` | `fossil ls` |
| Current revision | `svn info --show-item revision` | `hg id -i` | `p4 changes -m1 -s submitted` | `fossil info` |
| Tree cleanliness | `svn status -q` | `hg status -q` | `p4 diff -sr` | `fossil changes` |
| Submodule equiv | svn:externals | subrepos | branches/streams | no direct equiv |
| File tracked? | `svn info <file>` | `hg files <file>` | `p4 fstat <file>` | `fossil ls <file>` |

## The abstraction (when needed)

```go
type VCS interface {
    // TrackedFiles returns all files under version control.
    TrackedFiles(dir string) ([]string, error)

    // CurrentRevision returns the current commit/changeset ID.
    CurrentRevision(dir string) (string, error)

    // IsClean reports whether the working tree matches the current
    // revision with no uncommitted changes.
    IsClean(dir string) (bool, error)

    // ExternalDeps returns nested repository references (submodules,
    // svn:externals, subrepos) with their pinned revision.
    ExternalDeps(dir string) ([]ExternalDep, error)

    // IsTracked reports whether a specific file is under version control.
    IsTracked(dir, file string) (bool, error)
}

type ExternalDep struct {
    Path     string // directory path
    Revision string // pinned revision ID
}
```

The git implementation is ~60 lines. Each additional VCS is another
~60 lines. The call sites are already isolated to six functions.

## What breaks without git

| Feature | Impact | Mitigation |
|---|---|---|
| **Commit receipts** | requires "revision ID implies tree content" — true for content-addressed VCS (git, hg, fossil), NOT for delta-based (SVN, CVS, Perforce) | fall back to content-path receipts only |
| **Gitlink pseudo-inputs** | git-specific concept | each VCS has its own external-dep mechanism; the interface covers it |
| **`.gitignore` parsing** | each VCS has its own ignore syntax | the pattern-matching approach generalizes; only the parser changes |
| **`safe.directory` workaround** | git-specific ownership check | other VCS don't have this issue |

## Design principle

Don't call git directly from feature code — call through the five
capability functions above. When a second VCS is needed, implement the
interface and the feature code doesn't change. The current code
violates this in places (git commands are inlined); the refactor is
mechanical but not urgent until a real second-VCS use case exists.

## The commit receipt soundness note

Commit receipts are the one feature that depends on a deep VCS
property: **a revision ID uniquely determines tree content**. This is
true for content-addressed VCS (git, Mercurial, Fossil) by
construction. For delta-based VCS (SVN, Perforce, CVS), a revision
number determines content only within a single repository instance —
the same number in a different checkout could have different content
if the server was restored from backup, etc. For those VCS, commit
receipts would need the additional guarantee that the repository URL
is part of the key.
