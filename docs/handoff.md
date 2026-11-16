# Handoff configuration

[中文](handoff_CN.md)

## Design rationale

Account ownership is durable and keyed by authenticated caller scope plus session family. The existing isolation plugin authorizes the pool first. Compaction is a boundary signal, not permission to select an account outside that pool.

The client reports compaction start/completion; the gateway verifies adoption of the matching normalized summary, waits for in-flight work, and buffers B's first response. B becomes the owner only after complete success. A failed trial keeps A and requires an explicit recovery action.

## Prerequisites

- Build the matching gateway and key-account-bind plugin.
- Give one business key at least two explicitly authorized accounts.
- Each candidate must be available, have known five-hour/weekly quotas and at least 20% remaining, and support all recorded family routes.
- The validated inference client is Claude Code 2.1.309 using the sonnet-4-6 Messages SSE path. Do not infer Codex CLI support from the upstream gateway's other protocols.

## Caller scope

Scopes are generated from the business key using the same versioned algorithm as the gateway. They are not account IDs.

```bash
python -c 'import getpass,hashlib; key=getpass.getpass("Client SK: ").strip(); print(hashlib.sha256(("cli-proxy-api:caller-scope:v1\0"+key).encode()).hexdigest())'
```

Set these fields in the existing configuration:

```yaml
routing:
  handoff:
    enabled: true
    caller-scopes:
      - "replace-with-the-generated-scope"
    state-file: "./state/handoff.db"
```

Restart the gateway after changing these settings. Do not share one state database between multiple processes.

## Client hooks

From the client project directory, copy this repository's `client/handoff_hook.py` to `.claude/hooks/relay_handoff.py`. Merge `client/settings.example.json` into the project's `.claude/settings.json`; do not overwrite unrelated settings.

The sample hook command is project-relative. Start the client at the project root. If your client starts elsewhere, adapt the command to that client's supported project-root mechanism.

Supply the environment to the client process:

| Variable | Value |
| --- | --- |
| `HANDOFF_BASE_URL` | Gateway origin, such as `http://127.0.0.1:8317`; do not append `/v1`. |
| `HANDOFF_API_KEY` | The same business key used for inference. |
| `HANDOFF_OUTBOX_DIR` | A private directory for notification retries; optional. |

The hook requires Python 3.11+. On Windows, use `python`; on systems providing only `python3`, update the sample command accordingly.

Both automatic and manual compaction notify the gateway. A missing receipt or unknown context shape does not authorize a switch.

## Default behavior

| Condition | Result |
| --- | --- |
| No eligible B | Keep A; do not inflate usage. |
| Usage reaches 80% | Reserve a candidate and wait for compaction. |
| Usage reaches 90% | Raise reported input usage for one eligible main response to trigger native compaction. |
| Usage reaches 97% | Hook blocks normal prompts and requests compaction. |
| A actually exhausted/unavailable | Return an error; no automatic cross-account fallback. |
| B succeeds completely | Commit B and advance the generation. |
| B fails | Retain A, cool down the failed candidate, wait for recovery. |

The percentages are compiled defaults from `internal/handoff/engine.go`; the YAML currently exposes enablement, scopes, and state path only.

The trigger raises input usage to at least 180000 for the tested model. Later `message_delta` events repeating input usage retain the same floor for that response. Other responses, authoritative quota values, output tokens, and cache fields remain separate.

## API

All endpoints use normal business-key authentication and infer the scope server-side.

| Endpoint | Purpose |
| --- | --- |
| `GET /v1/handoff/status?session_id=...` | Bound account, generation, phase, quota, candidate readiness, thresholds. |
| `POST /v1/handoff/pre-compact` | Record compression start for a generation. |
| `POST /v1/handoff/compact` | Submit the matching PostCompact summary receipt. |
| `POST /v1/handoff/manual` | Explicit recovery or handoff request. |

Control bodies use `session_id`, `generation`, `notice_id`, `compact_summary` as applicable. Manual requests use `session_id` and `action`.

Actions:

- `prepare` requests a handoff without falsifying actual quota.
- `trigger` requests a handoff with a native-compaction trigger on the next eligible response.
- `retry` permits another bounded attempt after failure.
- `cancel` cancels the pending handoff; it does not restore an exhausted account.
- `acknowledge-drain` is for expired leases owned by a previous process, after the operator verifies it has stopped. It cannot release a lease owned by the current process.

These are operational controls, not substitutes for account eligibility, authorized pools, or summary evidence.

## Retention and limits

State is plaintext BoltDB. The first successful B reply may be retained for five minutes for same-request replay, up to 1 MiB per response and approximately 64 MiB total. Periodic cleanup is logical expiry, not secure erasure.

The current gateway has a 90-second response deadline, 1 MiB trial buffer, 16 MiB request cap, and 10-second SSE heartbeat. Heartbeats do not extend total client deadlines. Persisted bindings are separate from the upstream one-hour affinity cache.

Old child contexts and signed/opaque state are rejected unless their own adoption requirements are satisfied. Do not delete a database merely to force a new account assignment.
