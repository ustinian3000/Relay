"""Run an isolated native client against the mocked core integration server."""
import json
import os
from pathlib import Path
import queue
import subprocess
import sys
import tempfile
import threading
import time
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parents[2]
REPORT_ROOT = Path(os.environ.get("HANDOFF_REPORT_DIR", str(ROOT / "test-reports" / "native-client")))


def main():
    base, case, fixture = sys.argv[1:4]
    assert base.startswith("http://127.0.0.1:")
    binary = os.environ.get("HANDOFF_CLIENT_BINARY")
    if not binary:
        raise RuntimeError("Set HANDOFF_CLIENT_BINARY to a Claude Code 2.1.309 executable.")
    version = subprocess.run([binary, "--version"], check=True, capture_output=True, text=True).stdout
    if not version.strip().startswith("2.1.309"):
        raise RuntimeError("This native probe requires the validated Claude Code 2.1.309 client.")
    REPORT_ROOT.mkdir(parents=True, exist_ok=True)
    session = str(uuid.uuid4())
    report = {"case": case, "client_version": "2.1.309", "real_accounts_used": False, "turns": []}
    with tempfile.TemporaryDirectory(prefix="native-" + case + "-", dir=REPORT_ROOT, ignore_cleanup_errors=True) as folder:
        work = Path(folder)
        command = '"' + Path(sys.executable).as_posix() + '" "' + (ROOT / "client/handoff_hook.py").as_posix() + '"'
        settings = work / "settings.json"
        settings.write_text(json.dumps({"hooks": {
            event: [{"matcher": "auto|manual", "hooks": [{"type": "command", "command": command}]}]
            if event != "UserPromptSubmit" else [{"hooks": [{"type": "command", "command": command}]}]
            for event in ("PreCompact", "PostCompact", "UserPromptSubmit")
        }}), encoding="utf-8")
        env = dict(os.environ)
        for key in (
            "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK",
            "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE",
            "DISABLE_AUTO_COMPACT", "DISABLE_COMPACT",
        ):
            env.pop(key, None)
        env.update({
            "CLAUDE_CONFIG_DIR": str(work / "isolated-config"),
            "ANTHROPIC_API_KEY": "key-a", "ANTHROPIC_BASE_URL": base,
            "HANDOFF_BASE_URL": base, "HANDOFF_API_KEY": "key-a", "HANDOFF_OUTBOX_DIR": str(work / "outbox"),
            "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "DISABLE_TELEMETRY": "1", "API_TIMEOUT_MS": "10000",
        })
        args = [binary, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
                "--model", "claude-sonnet-4-6", "--tools", "Read" if case == "tool" else "",
                "--setting-sources", "user", "--settings", str(settings), "--session-id", session]
        if case == "tool":
            args += ["--allowedTools", "Read"]
        proc = subprocess.Popen(args, cwd=work, env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, text=True, encoding="utf-8")
        output = queue.Queue()
        stderr_lines = []

        def read_output():
            for line in proc.stdout:
                try:
                    output.put(json.loads(line))
                except ValueError:
                    pass
        threading.Thread(target=read_output, daemon=True).start()
        threading.Thread(target=lambda: stderr_lines.extend(proc.stderr.readlines()), daemon=True).start()

        def turn(label, text):
            proc.stdin.write(json.dumps({"type": "user", "message": {"role": "user", "content": text}}) + "\n")
            proc.stdin.flush()
            until = time.monotonic() + 45
            while time.monotonic() < until:
                try:
                    event = output.get(timeout=.2)
                except queue.Empty:
                    if proc.poll() is not None:
                        raise RuntimeError("native client exited: " + "".join(stderr_lines)[-2000:])
                    continue
                if event.get("type") == "result":
                    row = {"turn": label, "is_error": bool(event.get("is_error")), "subtype": event.get("subtype"),
                           "result": event.get("result", "")[:200]}
                    report["turns"].append(row)
                    if row["is_error"]:
                        raise RuntimeError("native result failed: " + json.dumps(row))
                    return
            raise TimeoutError("native client result timeout")
        try:
            initial = "Remember ORIGINAL_PRIVATE_CONTEXT_882 and reply briefly."
            if case == "tool":
                initial = "Remember ORIGINAL_PRIVATE_CONTEXT_882. Use Read on " + fixture + " and report its marker."
            turn("initial", initial)
            for index in range(3):
                turn("followup-" + str(index + 1), "Continue the synthetic task. Reply briefly.")
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
            req = urllib.request.Request(base + "/v1/handoff/status?session_id=" + session, headers={"Authorization": "Bearer key-a"})
            with opener.open(req, timeout=5) as response:
                report["state"] = json.load(response)
            assert report["state"]["generation"] == 1, report
            assert not list((work / "outbox").glob("*.json")), "unacknowledged compaction"
        finally:
            proc.stdin.close()
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.terminate()
                proc.wait(timeout=5)
            report_dir = REPORT_ROOT
            (report_dir / ("native-" + case + "-client.json")).write_text(json.dumps(report, indent=2), encoding="utf-8")
    print(json.dumps(report), flush=True)


if __name__ == "__main__":
    main()
