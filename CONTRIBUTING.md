# Contributing to Orbit

Thanks for looking at Orbit. This repo is a local MCP server for macOS +
[OrbStack](https://orbstack.dev). Unit tests do not need OrbStack; the smoke
test and Docker E2E path do.

## Setup

1. Install [Go 1.25+](https://go.dev/dl/).
2. Clone the repo and run the unit suite:

```sh
git clone https://github.com/machugram/orbit.git
cd orbit
go test ./...
go vet ./...
```

3. Optional real-VM smoke (macOS + OrbStack):

```sh
go run ./cmd/smoke
```

## What we look for in a change

- `gofmt -w` on touched Go files (`gofmt -l .` must be empty).
- `go test ./...` and `go vet ./...` pass.
- Secret values never land in argv, audit logs, or `*_list` tool results.
- New MCP tools are added to `TestAllSpecToolsRegistered` in
  `internal/tools/tools_test.go`.
- Architectural choices get an ADR under `docs/adr/` (see below).

Open a pull request against `main`. Use the PR template checklist.

## Architecture decision records

ADRs live in [`docs/adr/`](docs/adr/). They use a Status Quo → Goal →
Non-Goal → Proposed Solution → Alternatives shape.

If you use Cursor, [`skills/adr/SKILL.md`](skills/adr/SKILL.md) is an optional
prompt that writes that shape. It is contributor tooling, not runtime code.
You do not need Cursor to add an ADR — copy an existing file in `docs/adr/`.

## Security reports

Do not file a public issue for an unreleased vulnerability. Follow
[SECURITY.md](SECURITY.md).

## Scope notes

- Orbit is macOS + OrbStack by design. Cross-platform VM backends are out of
  scope unless an ADR says otherwise.
- The Homebrew formula in `Formula/orbit.rb` is a template with placeholder
  SHA256s. Do not treat it as installable until a tagged GitHub Release
  exists and those hashes are filled in (`RELEASE.md`).
