# Installing Orbit

Orbit is distributed as pre-built binaries for macOS (arm64 and x86_64) and via Homebrew.

**Requirements:**
- macOS (Sonoma or later recommended)
- [OrbStack](https://orbstack.dev) installed and running
- [Claude Desktop](https://claude.ai/download) or VS Code with Claude extension

## Option 1: GitHub Release (Direct Binary)

1. Go to [GitHub Releases](https://github.com/anthropics/orbit/releases) and download the binary for your architecture:
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

## Option 2: Homebrew

Once the formula is published to a tap:

```sh
brew tap anthropics/orbit
brew install orbit
```

Then register with Claude:
```sh
claude mcp add orbit -- $(which orbit)
```

## Verifying the Installation

After registration, you should see `orbit` listed in Claude's MCP servers:

**Claude Desktop:** Settings → Features → MCP servers → "orbit" (enabled)

**VS Code:** Extensions → Claude → MCP Servers → "orbit" listed

If Orbit fails to start, check:
- OrbStack is running: `orbctl list` should work
- Docker is accessible: `docker ps` should work
- `ORBIT_VAULT_PATH` and `ORBIT_AUDIT_PATH` directories are writable (they default to `~/.orbit/`)

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

**Via GitHub Release:**
- Download the latest binary from [Releases](https://github.com/anthropics/orbit/releases) and replace the old one.

**Via Homebrew:**
```sh
brew upgrade orbit
```

Then restart Claude Desktop or your editor.
