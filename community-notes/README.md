# Community notes

Findings from building Orbit, a local MCP server on OrbStack. Kept only where the obvious API succeeds and the machine is still wrong.

| Note | What it covers |
|---|---|
| [01-technical-field-notes.md](01-technical-field-notes.md) | Six field notes with the APIs, timings, and the code that handles them |
| [02-twitter-x-threads.md](02-twitter-x-threads.md) | One 7-post thread and two standalone posts |
| [03-linkedin-posts.md](03-linkedin-posts.md) | Systems post and product/architecture post |

1. `orbctl create --cloud-init` is a no-op on Alpine, Fedora, Arch, and Void.
2. OpenSSH rejects a valid key while `/etc/shadow` says `user:!`.
3. The multiplexer at `127.0.0.1:32222` ignores in-VM `authorized_keys`.
4. `docker run -e NAME=value` leaks into argv. Bare `-e NAME` does not. SSH uses `sh -s` plus stdin.
5. `docker compose exec -f -` deadlocks because the file and the command share stdin.
6. A destroyed VM is an `UPDATE` on a primary key, so the same name cannot be inserted again. Teardown needs its own 30-second context.
