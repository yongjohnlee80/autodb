# internal/commentguard

A static analysis ratchet and test suite that guarantees that code comments across `autodb` remain accessible, professional, and citable by the public.

---

## The Rule: Public Accessibility

A code comment may cite only:
1. A public URL accessible on the open internet (e.g. RFC specifications, official database documentation).
2. A file located directly within this repository (e.g. `docs/front-door/protocol-matrix.md`).

Any reference to internal project management items, unlinked architecture decision records, private pull request numbers, or review round markers is strictly disallowed.

---

## Ratchet Architecture

`commentguard` maintains a list of **certified clean packages**:
```go
var certifiedClean = []string{
    "webserver",
    "core/config",
    "cmd/autodb",
    "rpc",
    "core/engine",
    "core/meta",
    "core/auth",
    "tui",
    "internal/vocabguard",
    "frontdoor",
    "core/exec",
}
```

Whenever a package is added to `certifiedClean`, `coordinates_test.go` performs a full AST traversal of every Go file and comment group in that package. Any unlinked internal coordinates cause the test suite to fail immediately, preventing regressions.
