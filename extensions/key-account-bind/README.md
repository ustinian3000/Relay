# Key account binding for Relay

This is the modified isolation plugin paired with the Relay gateway, derived from [cpa-key-account-bind](https://github.com/FFatTiger/cpa-key-account-bind).

It owns frontend business-key authentication and inspects the authorized account pool using the gateway's trusted `caller_scope`. A missing key, failed inspection, unknown account ID, or unhandled scope is rejected; it must not fall back to the unrestricted pool.

The plugin is a **separate Go module**. Building only the gateway does not build the plugin.

From this directory on Linux:

```bash
CGO_ENABLED=1 go build -buildmode=c-shared \
  -o ../../plugins/linux/amd64/key-account-bind.so .
go test ./...
```

On Windows, use a compatible C compiler and output `../../plugins/windows/amd64/key-account-bind.dll`. Match the gateway's OS/architecture.

Minimal integration:

```yaml
access:
  api-keys: []
plugins:
  enabled: true
  dir: "./plugins"
  configs:
    key-account-bind:
      enabled: true
      unbound: deny
      strategy: fill-first
      bindings:
        - key: "replace-with-your-private-business-key"
          allow:
            - "account-a.json"
            - "account-b.json"
```

Use exact credential IDs for a strict pool. Business keys must not also be configured in native `access.api-keys`. The global routing strategy and plugin strategy are separate settings.

The internal version marker remains `0.3.0-local-account-usage.4` for compatibility; it is not an upstream release claim. The MIT license in [LICENSE](LICENSE) is retained.
