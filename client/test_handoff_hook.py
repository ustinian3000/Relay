import http.server
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import unittest


class HookTests(unittest.TestCase):
    def setUp(self):
        self.folder = tempfile.TemporaryDirectory()
        self.state = {"generation": 0, "phase": "pending", "fiveHourUsedPercent": 92, "weeklyUsedPercent": 10,
                      "candidateReady": True, "reason": "", "softThreshold": 80, "hardThreshold": 97}
        self.records = []
        self.fail_notice = False
        owner = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                self.send_response(200)
                self.end_headers()
                self.wfile.write(json.dumps(owner.state).encode())

            def do_POST(self):
                payload = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                owner.records.append((self.path, payload))
                failure = self.path.endswith("/compact") and owner.fail_notice
                self.send_response(503 if failure else 200)
                self.end_headers()
                self.wfile.write(b'{"ok":true}')

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.env = dict(os.environ, HANDOFF_BASE_URL="http://127.0.0.1:" + str(self.server.server_port),
                        HANDOFF_API_KEY="synthetic-key", HANDOFF_OUTBOX_DIR=self.folder.name)

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.folder.cleanup()

    def hook(self, event, **extra):
        payload = dict(session_id="test-session", hook_event_name=event, **extra)
        result = subprocess.run([sys.executable, str(Path(__file__).with_name("handoff_hook.py"))],
                                input=json.dumps(payload), text=True, capture_output=True, env=self.env, check=True)
        return json.loads(result.stdout)

    def test_soft_and_hard_threshold_and_slash_escape(self):
        self.assertIn("systemMessage", self.hook("UserPromptSubmit", prompt="normal"))
        self.state["fiveHourUsedPercent"] = 98
        self.assertEqual(self.hook("UserPromptSubmit", prompt="normal")["decision"], "block")
        self.assertTrue(self.hook("UserPromptSubmit", prompt="/compact")["continue"])

    def test_lost_notification_outbox_and_original_generation(self):
        self.hook("PreCompact")
        self.state["generation"] = 7
        self.fail_notice = True
        self.hook("PostCompact", compact_summary="<summary>marker</summary>")
        notices = [row for path, row in self.records if path.endswith("/compact")]
        self.assertEqual(notices[0]["generation"], 0)
        self.assertTrue(list(Path(self.folder.name).glob("*.json")))
        self.fail_notice = False
        self.hook("UserPromptSubmit", prompt="resume")
        notices = [row for path, row in self.records if path.endswith("/compact")]
        self.assertEqual(notices[0], notices[1])
        self.assertFalse(list(Path(self.folder.name).glob("*.json")))

    def test_missing_precompact_never_sends_unscoped_notice(self):
        self.hook("PostCompact", compact_summary="marker")
        self.assertEqual(self.records, [])


if __name__ == "__main__":
    unittest.main()
