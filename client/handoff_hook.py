"""Native client hooks for the opt-in Relay handoff service (standard library).

Uses Claude Code's real hook event names. No credentials are stored in the outbox.
"""
import hashlib
import json
import os
from pathlib import Path
import sys
import urllib.error
import urllib.parse
import urllib.request
import uuid


def emit(value):
    print(json.dumps(value, ensure_ascii=False), flush=True)


def run():
    payload = json.load(sys.stdin)
    base = os.environ.get("HANDOFF_BASE_URL", "").rstrip("/")
    key = os.environ.get("HANDOFF_API_KEY", "")
    session = payload.get("session_id", "")
    if not base or not key or not session:
        emit({"continue": True, "systemMessage": "交接提醒未配置，请检查 HANDOFF_BASE_URL 与 HANDOFF_API_KEY。"})
        return
    parsed = urllib.parse.urlparse(base)
    if parsed.scheme not in ("http", "https") or not parsed.hostname:
        raise ValueError("invalid handoff URL")
    # Loopback test/control traffic must never be forwarded to a system proxy.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({})) if parsed.hostname in ("127.0.0.1", "localhost", "::1") else urllib.request.build_opener()
    root = Path(os.environ.get("HANDOFF_OUTBOX_DIR", str(Path.home() / ".codex-handoff")))
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    tag = hashlib.sha256((base + "\0" + key + "\0" + session).encode()).hexdigest()
    outbox = root / (tag + ".json")
    precompact = root / (tag + ".pre")

    def call(action, data=None):
        url = base + "/v1/handoff/" + action
        if data is None:
            url += "?session_id=" + urllib.parse.quote(session)
        request = urllib.request.Request(url, data=None if data is None else json.dumps(data).encode(),
                                         headers={"Authorization": "Bearer " + key, "Content-Type": "application/json"})
        with opener.open(request, timeout=5) as response:
            return json.load(response)

    def flush():
        if not outbox.exists():
            return
        record = json.loads(outbox.read_text(encoding="utf-8"))
        try:
            call("compact", record)
        except urllib.error.HTTPError as error:
            if error.code != 409:
                raise
            reason = json.load(error).get("error", {}).get("code")
            if reason != "stale_notice":
                raise
            # A committed handoff already advanced the generation.
        outbox.unlink(missing_ok=True)

    event = payload.get("hook_event_name", "")
    if event == "PostCompact":
        if not precompact.exists():
            emit({"continue": True, "systemMessage": "缺少本次压缩开始时的代次记录，保持原绑定，请检查 PreCompact Hook。"})
            return
        generation = json.loads(precompact.read_text(encoding="utf-8"))["generation"]
        record = {"session_id": session, "generation": generation, "notice_id": str(uuid.uuid4()),
                  "compact_summary": payload.get("compact_summary", "")}
        temp = outbox.with_suffix("." + str(uuid.uuid4()) + ".tmp")
        with temp.open("x", encoding="utf-8") as stream:
            os.chmod(temp, 0o600)
            json.dump(record, stream)
        temp.replace(outbox)
        precompact.unlink(missing_ok=True)
        flush()
        emit({"continue": True})
        return
    flush()
    status = call("status")
    if event == "PreCompact":
        call("pre-compact", {"session_id": session, "generation": status["generation"]})
        temp = precompact.with_suffix("." + str(uuid.uuid4()) + ".tmp")
        with temp.open("x", encoding="utf-8") as stream:
            os.chmod(temp, 0o600)
            json.dump({"generation": status["generation"]}, stream)
        temp.replace(precompact)
        emit({"continue": True})
        return
    if event != "UserPromptSubmit":
        emit({"continue": True})
        return
    prompt = payload.get("prompt", "")
    if isinstance(prompt, str) and prompt.lstrip().startswith("/"):
        emit({"continue": True})
        return
    used = max(status.get("fiveHourUsedPercent") or 0, status.get("weeklyUsedPercent") or 0)
    if used >= status["hardThreshold"]:
        emit({"decision": "block", "reason": "当前绑定账号额度即将耗尽，请先输入 /compact；若已耗尽，请等待额度恢复。"})
    elif status["phase"] == "blocked":
        emit({"decision": "block", "reason": "交接已暂停：" + status.get("reason", "") + "。请检查后重试或取消交接。"})
    elif used >= status["softThreshold"]:
        text = "当前账号额度已用约 " + str(round(used)) + "%。"
        text += "已等待自动压缩，也可输入 /compact。" if status["candidateReady"] else "当前没有满足额度条件的备用账号，请留意剩余额度。"
        emit({"continue": True, "systemMessage": text})
    else:
        emit({"continue": True})


if __name__ == "__main__":
    try:
        run()
    except urllib.error.HTTPError as error:
        # A session has no binding until its first inference request.
        try:
            code = json.load(error).get("error", {}).get("code")
        except (ValueError, OSError):
            code = None
        if code == "unknown_session":
            emit({"continue": True})
        else:
            emit({"continue": True, "systemMessage": "交接状态暂时无法确认；压缩通知会在下次提交时重试。"})
    except (OSError, ValueError, KeyError):
        emit({"continue": True, "systemMessage": "交接状态暂时无法确认；压缩通知会在下次提交时重试。"})
