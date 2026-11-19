# Proxy Account Gate

## Purpose

The local Claude/Headroom deployment uses one fixed outbound proxy per account. A temporary residential-proxy failure must not stop the whole Relay service, trigger direct fallback, or send the request through an unintended account or proxy.

## Behavior

- Relay and Headroom stay running during a proxy outage.
- The affected auth file is temporarily marked `disabled: true`.
- The original auth file is backed up under `runtime/local-claude-test/disabled-auths/`.
- Requests are rejected by the existing account-pool isolation instead of being sent through a fallback route.
- The guard validates the proxy binding and egress IP every 30 seconds.
- After the proxy recovers, the original auth file is restored automatically.
- A changed proxy binding is never restored automatically; the account remains disabled until it is reviewed.

## Deployment

Copy these scripts into `runtime/local-claude-test/` in the Relay deployment:

- `start.ps1` starts Relay and the account-level proxy guard.
- `watch.ps1` monitors each account independently.
- `run.ps1` is the foreground-compatible runner.

Start the local services with:

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\runtime\local-claude-test\start.ps1
```

The guard does not log proxy credentials. Inspect `runtime/local-claude-test/guard-failure.txt` for the affected auth filename and failure reason.

## Validation

The simulated egress-mismatch test passed:

1. Relay `8318` remained listening.
2. Headroom `8788` remained available.
3. Only the affected account was disabled.
4. The account automatically recovered after the expected egress baseline was restored.
5. The temporary disabled-auth backup was removed after recovery.

The runtime scripts are intentionally copied into the tracked deployment directory because the local `runtime/` directory is ignored by Git.