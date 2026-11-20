# Relay

**A stateful AI gateway for scoped routing and deliberate session handoff.**

[中文说明](README_CN.md) · [Handoff guide](docs/handoff.md) · [Web tools](web/README.md) · [Validation](docs/validation.md)

Relay is a downstream development version of [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). It adds caller-scoped account pools, quota visibility, persistent handoff state, and a client-assisted boundary for transferring a compacted session between authorized accounts.

This is a source repository, not a portable installer. Handoff is experimental and disabled by default.

## Features

| Component | Purpose |
| --- | --- |
| Compatible gateway | Retains upstream model providers and protocol adapters. |
| Account isolation | A frontend key selects an explicitly authorized account pool; failed inspection never falls back to the full pool. |
| Session affinity | Keeps related requests on their assigned credential. |
| Quota visibility | `GET /v1/account-usage` reports only the authenticated key's pool. |
| Compaction handoff | Persists ownership and generation; confirms B only after its first complete successful response. |
| Client hooks | Reports compaction boundaries, retries lost notifications, and provides quota reminders. |
| Local web tools | Account/SK planning, read-only remote inspection, quota views, and an enrollment wizard. |

The handoff path is narrower than the general gateway: the validated client is **Claude Code 2.1.309**, with **claude-sonnet-4-6 over Messages HTTP SSE**. Codex CLI handoff has not been validated. Provider names in code identify the actual protocol.

## Layout

```text
cmd/server/                    Gateway executable
internal/handoff/              State machine, context evidence, SSE gateway
internal/api/handoff.go        Authentication and inference integration
sdk/cliproxy/auth/             Pool authorization, selection, quota lookup
extensions/key-account-bind/   Isolation plugin, a separate Go module
client/                       Native-client hook and settings example
web/                          Account manager and enrollment wizard
tools/testing/                Synthetic native-client integration probe
docs/                         Guides, validation, upstream SDK documentation
licenses/                     Preserved upstream notices
```

The upstream Go module path is retained to avoid rewriting internal imports.

## Build and start

Requirements: **Go 1.26.10+**, a C compiler for CGO/native plugins, and **Python 3.11+** for optional hooks/web tools. Gateway and plugin must target the same OS and architecture.

Linux, from the repository root:

```bash
mkdir -p bin plugins/linux/amd64 auths
CGO_ENABLED=1 go build -o bin/relay ./cmd/server
(
  cd extensions/key-account-bind
  CGO_ENABLED=1 go build -buildmode=c-shared \
    -o ../../plugins/linux/amd64/key-account-bind.so .
)
cp config.relay.example.yaml config.yaml
```

Before starting, replace the private business-key placeholder in plugin bindings, set exact authorized credential IDs in `allow`, and put those credential files in `auths/`. No credentials are included. Keep `access.api-keys` empty for plugin-only business authentication.

```bash
./bin/relay --config config.yaml --local-model
```

The Relay example binds to `127.0.0.1:8317`. The inherited `config.example.yaml` is a broader upstream template with different defaults.

On Windows, set `CGO_ENABLED=1` and build `bin/relay.exe` plus `plugins/windows/amd64/key-account-bind.dll` using a compatible C compiler. Use `go build -buildmode=c-shared` for the plugin.

The inherited Docker scripts still contain upstream image defaults. This repository does not publish a Relay image; the source-build instructions above are the supported starting point.

For a shared deployment, see [Nginx and public IP HTTPS](docs/nginx-https.md) for a trusted certificate and automatic renewal without a domain name.

## Optional Antigravity OAuth

For Antigravity login and token refresh, set `ANTIGRAVITY_OAUTH_CLIENT_ID` and
`ANTIGRAVITY_OAUTH_CLIENT_SECRET` in the server environment or its local `.env`
file before starting. Use an OAuth client you are authorized to use, with the
required scopes and callback URI. See [.env.example](.env.example).
No OAuth client credentials are embedded in the source. Missing configuration
rejects Antigravity login/refresh with a configuration error; other providers
and session handoff are unaffected. Existing Antigravity refresh tokens must
match the configured OAuth client; otherwise, authorize the account again.

## Enable handoff

Handoff is opt-in per caller scope and requires an authorized, quota-known B with sufficient capacity. Read the [handoff guide](docs/handoff.md) before enabling `routing.handoff` or installing hooks.

Normal affinity and strict handoff are different modes. Outside the opt-in scope list, existing affinity/failover configuration remains effective.

## Web tools

Run each application in a separate terminal:

```bash
python -m pip install -r web/account-manager/requirements.txt
python web/account-manager/app.py --core bin/relay.exe --port 18766
python web/claude-login/app.py --port 18765
```

Use `bin/relay` on Linux. Both applications bind to loopback. They do not automatically start with the Go server. See [web/README.md](web/README.md) for remote-layout assumptions and local data storage.

## Tests

```bash
go test ./internal/handoff
go test ./internal/api -run 'TestHandoff(Core|Routes)'
go test ./sdk/cliproxy/auth -run 'Test(ScopedPluginAffinity|AccountUsage)'
go test ./internal/config -run TestHandoffConfig
go test ./...
python -m unittest discover -s client -p "test_*.py"
python -m unittest discover -s web/account-manager -p "test_*.py"
python -m unittest discover -s web/claude-login -p "test_local_app.py"
python -m unittest discover -s web/claude-login -p "test_ssh_transport.py"
```

Install `requirements-dev.txt` for development tests. The remote-helper tests depend on Linux `fcntl`; on Linux run `python -m unittest discover -s web/claude-login -p "test_*.py"` to include them.

The full suite includes inherited upstream tests. Native-client mock integration is opt-in and uses synthetic credentials; see [validation](docs/validation.md).

## Limits

- Opted-in inference requires the tested native client and Messages SSE; other inference routes are rejected.
- Incompatible opaque state, including signed thinking blocks, cannot cross accounts.
- Old child sessions may require their own compaction evidence or a new session.
- The response gateway has a 90-second deadline and a 1 MiB first-B buffer.
- Usage inflation can distort client token/cost displays; authoritative quota queries remain separate.
- State and short-lived response replays are not encrypted at rest. Keep their directory private.
- Handoff scope/path changes require a restart.

Use provider integrations only where your account and agreement authorize them. Technical routing and successful tests do not establish permission or guarantee account availability.

## Origin and licenses

Relay-specific additions use the existing repository [Apache-2.0 license](LICENSE), subject to preserved component licenses. CLIProxyAPI-derived source retains its [MIT notice](licenses/CLIProxyAPI-MIT.txt); the plugin retains its [MIT license](extensions/key-account-bind/LICENSE). See [NOTICE](NOTICE).

Original upstream introductions are preserved as [English](README_UPSTREAM.md), [Chinese](README_UPSTREAM_CN.md), and [Japanese](README_UPSTREAM_JA.md). Their upstream feature/release claims are not Relay validation claims.
