# Validation scope

[中文](validation_CN.md)

## Recorded results

On November 15, 2026, an independent test instance completed real two-account tests for manual compaction handoff and gateway-triggered native auto-compaction handoff. In each case, B recalled four facts introduced to A, and another turn stayed on B with generation 1.

The validated combination was Claude Code 2.1.309, claude-sonnet-4-6, and Messages HTTP SSE. The test used short text main sessions, one direction, and responses with zero thinking tokens. Production configuration and credential-file hashes were unchanged; the temporary key, credential copies, and instance were removed.

A live-stream difference exposed a bug: final `message_delta.usage.input_tokens` could replace the inflated initial value. This was reproduced with the native client against a mock upstream, then fixed by applying the same input floor across usage updates in the chosen response.

The subsequent local regression recorded 24 passing events, including subtests, with no failures and race detection enabled. Earlier simulated testing also covered persistence, isolation, concurrency, stale requests, lost notices, canceled/incomplete streams, candidate revocation, and bounded retries.

Private account names, endpoint details, SSH runners, raw live messages, and original reports are intentionally not part of this source repository.

## Repeat local tests

On November 17, 2026, the Antigravity OAuth environment-configuration change
passed targeted tests across internal auth, SDK auth, management, and runtime
executor packages, followed by a Windows gateway build. Mock transports verified
configured credentials and rejection before network access when configuration
is missing. No real accounts were used for these checks. CI includes this suite.

See the root README for unit tests. Add `-race` when a compatible C toolchain is available. Test the separate plugin module from `extensions/key-account-bind`.

To opt into native-client integration, point `HANDOFF_NATIVE_TEST_SCRIPT` at `tools/testing/native_client_probe.py` and `HANDOFF_CLIENT_BINARY` at a Claude Code 2.1.309 executable. Then run:

```bash
go test ./internal/api -run '^TestHandoffNativeClient$' -count=1 -v
```

The Go test starts a loopback server with synthetic accounts and a mock executor. The probe verifies the client version and uses isolated client settings. Output defaults to `test-reports/native-client`; `HANDOFF_REPORT_DIR` can override it. No real credentials are needed.

## Not established

The tests do not establish Codex CLI handoff, other native-client versions, signed thinking transfer, reverse-direction live handoff, long-running live tool chains, transparent migration of every old subagent, or provider-policy approval. The live test requested handoff explicitly; it did not exhaust real accounts to naturally reach all quota thresholds.
