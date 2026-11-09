# LinkedIn posts

Copy as-is.

## Post 1 — Systems

I spent the last stretch making a local agent control plane on OrbStack. The APIs that look like the integration are the ones that fail quietly.

1. **Cloud-init is Debian-only.** `orbctl create --cloud-init` succeeds on Alpine, Fedora, Arch, and Void, and the user-data is dropped. Root that works on every image is `orb -m <vm> -u root sh -s`, retried 5 times, 3 seconds apart, for up to 3 minutes, until stdout contains `ORBIT_PROVISIONED`.

2. **A locked password blocks pubkey auth.** `orbctl` writes `agent:!` in `/etc/shadow`. OpenSSH rejects a valid `authorized_keys`. Rewriting `!` to `*` keeps passwords disabled and lets the key through.

3. **The SSH multiplexer is a single shared credential.** `127.0.0.1:32222` authenticates with `~/.orbstack/ssh/id_ed25519` and never reads the VM's `authorized_keys`. The error is `unable to authenticate, attempted methods [publickey]`. Per-VM keys have to dial `<vm>.orb.local:22`.

4. **`-e NAME=value` is a process-table leak.** Bare `-e NAME` copies the variable from the Docker subprocess environment. The same rule over SSH: remote argv is `sh -s`, and the secret moves as `export` on stdin.

5. **`docker compose exec -f -` deadlocks on stdin**, and a soft-deleted VM row with `id TEXT PRIMARY KEY` makes "destroy" and "create again" contradict each other. Teardown also needs a fresh 30-second context. The command timeout has already cancelled the one you meant to reuse.

The pattern: the convenient flag returns success, and the failure shows up a minute later as "SSH never came up."

## Post 2 — Product / architecture

An agent does not want 40 verbs that mirror `orbctl` and `docker`. It wants a handful of jobs: run this, keep this machine, publish this service, put this file, store this secret.

Getting to 9 tools (`run`, `workspace_open`, `workspace_close`, `service_up`, `service_down`, `file_put`, `file_get`, `secret_put`, `converge`) was the product cut. The engineering cut was accepting that OrbStack's convenience layer cannot be the product.

Cloud-init does not provision Alpine. The multiplexer at port 32222 will not use the key you just installed. `docker run -e TOKEN=…` puts the token in `ps` and in the model transcript. `docker compose exec` cannot share stdin with an inline compose file. A destroyed row that lookup hides is still a primary key, so the same workspace name cannot come back.

So the server owns the awkward parts. It provisions over a root shell, dials `<name>.orb.local:22` with a key the model never sees, injects secrets by name, and reports a command's exit code as data. The model sees a workspace, an address, and stdout. That split is what makes a 9-tool surface honest instead of a thinner CLI.
