import json

from support import ShellTest


class SlackTest(ShellTest):
    def payload(self, **env):
        result = self.run_script("slack-payload.sh", **env)
        self.assertEqual(result.returncode, 0, result.stderr)
        payload = json.loads(result.stdout)
        for block in payload["attachments"][0]["blocks"]:
            if "text" in block:
                self.assertLessEqual(len(block["text"]["text"]), 150 if block["type"] == "header" else 3000)
            for field in block.get("fields", []):
                self.assertLessEqual(len(field["text"]), 2000)
            if block["type"] == "context":
                for field in block["elements"]:
                    self.assertLessEqual(len(field["text"]), 2000)
        return payload

    def test_long_failure_and_prelaunch_links(self):
        payload = self.payload(STATE="fail", REASON="bad" * 2000, EXCERPT="error " * 2000,
                               TARGET_REF="x" * 3000, BUCKET="b", RESULT_KEY="r/result.json")
        text = json.dumps(payload)
        self.assertNotIn("Box log", text)
        self.assertNotIn("Verdict on S3", text)
        self.assertIn("No box was launched", text)

    def test_reaper_long_and_empty(self):
        rows = [{"id": f"i-{i}", "runId": "123", "overdueMin": 50} for i in range(100)]
        self.payload(MODE="reaper", REAPED_JSON=json.dumps(rows))
        self.payload(MODE="reaper", REAPED_JSON="[]")

    def test_outcomes_remain_distinct(self):
        for state in ("ingested", "skipped", "failed"):
            payload = self.payload(STATE="ok", INGEST_STATE=state, INGEST_REASON="test reason")
            text = json.dumps(payload)
            self.assertIn("passed", text)
            if state != "ingested":
                self.assertIn("test reason", text)
        text = json.dumps(self.payload(STATE="fail", BOX_ID="i-123", BOX_RESCUED="true"))
        self.assertIn("left up for rescue", text)
        text = json.dumps(self.payload(STATE="fail", BOX_ID="i-123", BOX_RESCUED="false"))
        self.assertNotIn("was terminated", text)

    def test_post_failure_preserves_summary(self):
        self.stub("curl", 'printf "%s\\n" "$*" > "$HOME/curl-args"; exit 22')
        summary = self.work / "summary"
        result = self.run_script("post-slack.sh", STATE="fail", REASON="test failure",
                                 WEBHOOK="https://stub.invalid", GITHUB_STEP_SUMMARY=str(summary))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("slack notification failed", result.stdout)
        self.assertIn("test failure", summary.read_text())
        self.assertIn("--max-time 30", (self.work / "curl-args").read_text())

    def test_missing_webhook_never_calls_curl(self):
        self.stub("curl", 'exit 99')
        result = self.run_script("post-slack.sh", STATE="ok")
        self.assertEqual(result.returncode, 0)
        self.assertIn("unset; notification not delivered", result.stdout)
