# Security Policy

## Supported versions

Mainspring is pre-1.0. Security fixes land on the latest minor release only.

| Version | Supported |
|---|---|
| 0.2.x   | ✅ |
| < 0.2   | ❌ |

## Reporting a vulnerability

**Please do not open a public issue for a security problem.**

Use GitHub's private vulnerability reporting on this repository
(**Security → Report a vulnerability**). That opens a private advisory visible only to the
maintainers.

Please include: the version or commit, your platform, the configuration involved (redact any
keys), and the smallest reproduction you can manage.

You can expect an acknowledgement within **72 hours** and, for a confirmed issue, a fix or a
documented mitigation before public disclosure. This is a small project — that is a good-faith
commitment, not a contractual SLA.

## Threat model — read this before deploying

Mainspring is an inference control plane. Be clear about what it does and does not defend.

**When API keys are configured, it is designed to protect:**
- The served HTTP endpoint, via API keys mapped to named tenants and roles.
- Per-tenant resource use, via rate limits and token budgets.
- Administrative operations, which require the `admin` role.

Every one of those protections depends on having configured keys. With none configured, all three
are absent — see open mode below.

**It is explicitly not a hardened internet-facing gateway.** Deploy it behind your own trust
boundary — a private network, a VPN, or a reverse proxy that terminates TLS and authenticates
callers. Mainspring can terminate TLS itself, but it is not a substitute for an edge proxy.

**Running with no API keys configured is "open mode".** Every request is then unauthenticated and
unmetered, **including the `/admin` endpoints** — in open mode `requireAdmin` admits every caller,
so drain, reload, model load/unload, breaker reset and cache clear are all reachable by anyone who
can reach the port. This is a deliberate choice so a laptop install works with no setup, and
Mainspring warns at startup when it applies:

```
⚠  WARNING: no API keys configured — the server is OPEN (no authentication).
```

Exposing an open-mode instance to a network you do not control hands over both full use of your
hardware and administrative control of the server. Configure keys before binding to anything other
than loopback.

**Backends are subprocesses or local daemons.** Mainspring supervises engines such as
`llama-server` and adopts local daemons such as Ollama. It inherits their security properties; it
does not sandbox them. Model files are executed by those engines, so treat an untrusted model file
the same way you would treat untrusted code.

**Managed installs are verified, never silent.** Engine installation is opt-in, pinned by SHA-256,
and optionally checked against an ed25519-signed manifest. Mainspring never installs anything you
did not ask it to install.
