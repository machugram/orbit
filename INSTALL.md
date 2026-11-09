# Installing Orbit

**Requirements:**
- macOS (Sonoma or later recommended)
- [OrbStack](https://orbstack.dev) installed and running
- [Go 1.25+](https://go.dev/dl/) to build, or a pre-built binary
- An MCP client such as [Claude Code](https://docs.anthropic.com/en/docs/claude-code), Claude Desktop, or VS Code with the Claude extension

Until a GitHub Release exists, **build from source**.

## Option 1: Build from source (recommended)

```sh
git clone https://github.com/machugram/orbit.git
cd orbit
go build -o bin/orbit ./cmd/orbit
claude mcp add orbit -- $PWD/bin/orbit
```

Or, once the module is available:

```sh
go install github.com/machugram/orbit/cmd/orbit@latest
claude mcp add orbit -- $(go env GOPATH)/bin/orbit
```

## Option 2: GitHub Release (Direct Binary)

1. Go to [GitHub Releases](https://github.com/machugram/orbit/releases) and download the binary for your architecture:
   - **Apple Silicon (M1/M2/M3):** `orbit-aarch64-apple-darwin`
   - **Intel Mac:** `orbit-x86_64-apple-darwin`

2. Make it executable and place it somewhere convenient:
   ```sh
   chmod +x orbit-aarch64-apple-darwin
   mv orbit-aarch64-apple-darwin ~/bin/orbit  # or wherever you keep binaries
   ```

3. Register with Claude:
   ```sh
   claude mcp add orbit -- ~/bin/orbit
   ```

4. Restart Claude Desktop or your editor. Orbit should appear in the MCP servers list.

## Option 3: Homebrew

The in-tree [Formula/orbit.rb](Formula/orbit.rb) is a **template**. It is not
installable until a release is cut and the SHA256 placeholders are replaced
(see [RELEASE.md](RELEASE.md)). Until then, use Option 1.

Once the formula is published to a tap:

```sh
brew tap machugram/orbit
brew install orbit
```

Then register with Claude:
```sh
claude mcp add orbit -- $(which orbit)
```

## Codex / ChatGPT Work

I build or install the binary somewhere stable, then add Orbit to
`~/.codex/config.toml`:

```toml
[mcp_servers.orbit]
command = "/absolute/path/to/orbit"
startup_timeout_sec = 120
```

After I restart or reload Codex and the MCP server appears, I read `orbit://info`
to confirm which binary and paths are active, then read `orbit://doctor` to check
macOS, OrbStack, Docker, and vault/audit path readiness.

## Verifying the Installation

After registration, you should see `orbit` listed in Claude's MCP servers:

**Claude Desktop:** Settings → Features → MCP servers → "orbit" (enabled)

**VS Code:** Extensions → Claude → MCP Servers → "orbit" listed

**Codex / ChatGPT Work:** I should see Orbit tools such as `run`,
`workspace_open`, `service_up`, `secret_put`, and `converge`, plus resources
`orbit://info` and `orbit://doctor`.

If Orbit fails to start, check:
- OrbStack is running: `orbctl list` should work
- Docker is accessible: `docker ps` should work
- `ORBIT_VAULT_PATH` and `ORBIT_AUDIT_PATH` directories are writable (they default to `~/.orbit/`)
- `orbit_doctor` reports every check as `ok: true`

## Troubleshooting

**"orbit: command not found"**
- The binary isn't on your `PATH`. Either use the full path when registering with `claude mcp add`, or place it in a directory that's on your `PATH` (e.g., `~/bin` or `/usr/local/bin`).

**"keychain: set ORBIT_PASSPHRASE"**
- You're not on macOS. Orbit uses the macOS Keychain to store the vault passphrase. On other platforms, set the env var:
  ```sh
  export ORBIT_PASSPHRASE="your-passphrase"
  claude mcp add orbit -- /path/to/orbit
  ```

**"orbctl: command not found"**
- OrbStack isn't installed or its CLI isn't on your `PATH`. Install from [orbstack.dev](https://orbstack.dev).

**"docker: command not found"**
- OrbStack's Docker engine isn't available. Start OrbStack and ensure `docker ps` works.

## Updating

**From source:**
```sh
git pull && go build -o bin/orbit ./cmd/orbit
```

**Via GitHub Release:**
- Download the latest binary from [Releases](https://github.com/machugram/orbit/releases) and replace the old one.

**Via Homebrew:**
```sh
brew upgrade orbit
```

Then restart Claude Desktop or your editor.
