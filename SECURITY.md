# Security Policy

Buffarr is an independent homelab project, not a commercial product, but security
reports are welcome and taken seriously.

## Reporting a vulnerability

Please report suspected vulnerabilities privately (the repository's security
advisories, or directly to the maintainer) rather than opening a public issue, so
the problem can be addressed before it is widely known. Include what you found,
how to reproduce it, and the impact you expect.

## Design properties

Buffarr is single-operator and LAN-oriented. By construction:

- **Every web route is authenticated by default** (Steam OpenID, with a single
  allowlisted SteamID64). The public routes are a small, explicit allowlist.
- **The app and its session cookie are served over HTTPS.** Plain HTTP is bound
  to the host loopback only, so a session is never carried over the LAN in clear.
- **Secrets live in `.env`** (gitignored), never in the repository or its history.
- **The auto-generated TLS certificate is a plain server leaf**, not a CA, with a
  short validity.

## For operators and contributors

- Operator-side secret handling (rotation, keeping the Steam key out of `.env`) is
  documented in the "Securing secrets" section of
  [docs/INSTALL.md](docs/INSTALL.md).
- The contributor security checklist (no secrets in tracked files, new routes
  private by default, escaping user-influenced HTML) is in
  [CONTRIBUTING.md](CONTRIBUTING.md).
