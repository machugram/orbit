# ADR-007: Dependency minimization — `crypto/rand` over `uuid`, generic `sortedKeys`

**Status:** Accepted  
**Date:** 2026-06

## Context

Two patterns in the codebase pointed to the same class of problem: pulling in external packages or duplicating logic for operations that the Go standard library already covers.

### 1. UUID generation for credential IDs

`internal/vault/store.go` generated credential IDs with `uuid.NewString()` from
`github.com/google/uuid`. A UUID v4 is 122 bits of `crypto/rand` output encoded
as a hyphenated hex string. The full `uuid` package adds a direct dependency for
this single call site.

### 2. Deterministic map iteration (three duplicated sort blocks)

`internal/container/manager.go` needed to iterate over maps in a deterministic
order before building docker argument lists. The same pattern — collect keys,
sort, iterate — was written out three times independently: once for `RunSpec.Env`
(map[string]string), once for `RunSpec.SecretEnv` (map[string]string), and once
for `ComposeSpec.Scale` (map[string]int). Go 1.18+ generics exist precisely to
eliminate this kind of duplication without introducing a dependency.

## Decision

### 1. Replace `uuid.NewString()` with a local `newID()`

```go
// internal/vault/store.go
func newID() string {
    var b [16]byte
    rand.Read(b[:])
    return hex.EncodeToString(b[:])
}
```

`crypto/rand` and `encoding/hex` are already imported or present in the stdlib.
The result is a 32-character lowercase hex string with 128 bits of
cryptographically random entropy — identical collision resistance to UUIDv4's
122 bits, and more than sufficient for a credential ID namespace used by one
user on one machine. Drop `github.com/google/uuid` from the direct require block
in `go.mod`.

The `vm` package already used the same pattern (`randHex` in
`internal/vm/manager.go`). `newID()` adopts the same idea, keeping the codebase
consistent.

### 2. Add a generic `sortedKeys[V any]` helper

```go
// internal/container/manager.go
func sortedKeys[V any](m map[string]V) []string {
    keys := make([]string, 0, len(m))
    for k := range m {
        keys = append(keys, k)
    }
    sort.Strings(keys)
    return keys
}
```

Replace the three duplicated sort-and-iterate blocks with calls to `sortedKeys`.
No new package; uses the stdlib `sort` package already imported.

## Rationale

**External dependencies have a cost beyond their code.** Each direct dependency
is a supply-chain entry, a `go.sum` row to audit, and a potential source of
breaking changes. For operations the stdlib already covers at equivalent quality,
the right answer is always the stdlib.

**`crypto/rand` is strictly appropriate here.** Credential IDs need uniqueness,
not unguessability, but using a CSPRNG costs nothing extra and means the IDs are
fine as opaque capability tokens if the design ever extends in that direction.
Using `math/rand` would be a silent downgrade with no benefit.

**Generics are the right tool for `sortedKeys`.** Prior to Go 1.18, eliminating
the three duplicated sort blocks would have required either an `interface{}`
indirection (unsafe) or separate functions for each value type. With generics,
`sortedKeys[V any]` works for both `map[string]string` and `map[string]int` — it
is typed, efficient, and adds zero lines to the module's dependency graph.

**The `uuid` format is not required externally.** No external system receives
credential IDs. They are internal database primary keys used only within the
vault. There is no compatibility obligation to the UUID format, so the switch to
plain hex is consequence-free.

**Consistency with `vm.randHex`.** The `vm` package already produces
`crypto/rand` hex IDs for VM records and SSH key IDs. Unifying on the same
approach removes the one place where a different external package was used for
the same purpose.

## Consequences

- `github.com/google/uuid` is removed as a **direct** dependency. It may remain
  as an indirect transitive dependency if another direct dep pulls it in — that
  is outside our control and does not affect the binary's behavior.
- Credential IDs issued before this change (stored in an existing `vault.db`)
  remain valid — the store reads IDs as opaque strings and never re-derives them.
  There is no migration.
- The `sortedKeys` helper is unexported and package-private to
  `internal/container`. If deterministic map iteration is needed in other
  packages, the pattern can be replicated (it is three lines) or the helper can
  be moved to a shared `internal/util` package when there are two or more
  consumers. Today there is one.
- No behavior change. The only observable difference is in the format of newly
  issued credential IDs (hex without hyphens vs UUID with hyphens), which is
  irrelevant to any caller.
