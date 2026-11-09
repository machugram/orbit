# X posts

Copy as-is. Finding numbers match `01-technical-field-notes.md`.

## Thread

**1.**
Orbit is a local MCP server that gives an agent a machine on OrbStack. The happy path is `orbctl create --cloud-init` and `ssh` through OrbStack's multiplexer. Both are wrong, and neither one errors. Six things that showed up in the real control plane:

**2.**
`orbctl create --cloud-init` only does anything on Debian and Ubuntu. Alpine, Fedora, Arch, and Void accept the flag and boot with no sshd. The channel that actually has root everywhere is `orb -m <vm> -u root sh -s`. We retry that script 5 times, 3s apart, inside a 3-minute budget, and look for the stdout marker `ORBIT_PROVISIONED`.

**3.**
Installing `authorized_keys` is not enough. `orbctl` creates the user password-locked, so `/etc/shadow` says `agent:!`. OpenSSH rejects the key anyway. The one-line fix rewrites the lock to a disabled password and leaves pubkey auth on:

`sed -i "s/^$U:!*:/$U:*:/" /etc/shadow`

**4.**
OrbStack's SSH multiplexer is `127.0.0.1:32222`. It authenticates with `~/.orbstack/ssh/id_ed25519` and ignores `authorized_keys` inside the VM. A correct key still dies with `unable to authenticate, attempted methods [publickey]`. Dial `<vm>.orb.local:22` as `agent` with that VM's own Ed25519 key.

**5.**
`docker run -e POSTGRES_PASSWORD=s3cret` puts the secret in argv. `ps` can read it. Bare `-e POSTGRES_PASSWORD` tells Docker to copy the variable from its own environment. `DEMO_SECRET=x docker run --rm -e DEMO_SECRET alpine …` prints `x` and `x` never appears on the command line. Same idea over SSH: the remote command is `sh -s`, and `export TOKEN='…'` goes on stdin.

**6.**
`docker compose -f -` and `docker compose exec` share one stdin. Compose reads the YAML, then blocks waiting for the command's stdin. Resolve the service with `compose ps` and `docker exec` the real container (`myproject-api-1`), not the service name `api`. Also: `-f -` has no project dir, so a relative `build:` context runs against `$HOME`.

**7.**
Destroying a VM is an `UPDATE`, not a `DELETE`. `vms.id` is the primary key, so creating the same name again fails the insert while lookup already treats `state=destroyed` as missing. And if your `defer Destroy(ctx)` uses the command's context, the timeout has already cancelled it. Teardown needs its own 30s `context.Background()`.

## Standalone

**A.**
OpenSSH will refuse a correct pubkey when `/etc/shadow` contains `user:!`. OrbStack's `orbctl` creates the agent user that way. `authorized_keys` mode `0600` does not matter. Rewrite `!` to `*` (`sed -i "s/^$U:!*:/$U:*:/"`) so the account stays password-disabled and key auth works.

**B.**
`docker compose exec -f -` deadlocks. `-f -` consumes stdin for the compose file, and `exec` wants that same stdin for the command. After the YAML is read, the process just sits there. Look up the container with `compose ps` and call `docker exec myproject-api-1`. The service name `api` is not the container name.
