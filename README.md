# Orbit

An agent on a Mac needs a computer. OrbStack already runs the machines. Orbit is the surface the agent can use without operating `orbctl` and `docker` by hand.

It is a local MCP server with nine jobs: run a command, keep a workspace, publish a service, move a file, store a secret. The model receives a name, an address, and stdout. The private key and the secret value stay in a vault on the machine. A database password is a name in `secret_env`. The value is injected into the process and left out of the conversation and the audit log.

The constraint that shapes the product is the kernel. On Apple Silicon every OrbStack VM and container shares one. A container is the right place for a build, and the wrong place for untrusted code or for eBPF, which would attach to that shared kernel. The agent picks one of three:

- a container, in seconds
- an isolated VM, in about a minute
- a nested microVM with its own kernel, in minutes, when the work loads into the kernel

```text
workspace_open kind=vm distro=ubuntu name=lab
run target=lab command="uname -a"
```

A command with no workspace is a throwaway. `run` starts a container, returns the output, and removes it.

macOS and [OrbStack](https://orbstack.dev). One cgo-free Go binary. [MIT](LICENSE).

[Install](INSTALL.md) · [Tools and configuration](docs/USAGE.md) · [Design](docs/) · [Walkthrough](orbit-presentation.html)

Not affiliated with other projects named Orbit.
