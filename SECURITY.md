# Security

## Reporting a vulnerability

Report privately through **Security > Report a vulnerability** on the GitHub
repository. Do not open a public issue.

Include:

1. The version (`virticle version`).
2. What an attacker can do, and what access they need first.
3. Steps to reproduce.

Only the latest release gets fixes.

## What to know before exposing it

- The controller holds an SSH key that can manage every node. Protect its data
  directory and its host.
- It listens on `127.0.0.1` by default. Use HTTPS before opening it to a
  network; see [docs/web-tls.md](docs/web-tls.md).
- Prefer the scoped service account from `virticle node prepare` over root on
  the nodes; see [docs/install-node.md](docs/install-node.md).
- Release packages are not signed yet. Check downloads against `checksums.txt`.
