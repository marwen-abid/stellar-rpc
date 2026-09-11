import itertools
import json

from support import ROOT, ShellTest


class NotifyTest(ShellTest):
    def test_verdict_table(self):
        for states in itertools.product(("", "running", "ok", "fail"), repeat=4):
            with self.subTest(states=states):
                result = self.run_script("decide-verdict.sh", **dict(zip(("S1", "S2", "S3", "S4"), states)),
                                         VALIDATE_RESULT="success", LAUNCH_RESULT="success")
                self.assertEqual(result.returncode, 0)
                self.assertEqual(self.outputs()["state"], "ok" if "ok" in states else "fail")
                self.assertEqual(self.outputs()["rescued"], str("fail" in states and "ok" not in states).lower())
        for validate, launch, reason in (("failure", "skipped", "input validation failed"),
                                         ("success", "failure", "box launch failed")):
            self.run_script("decide-verdict.sh", S1="", S2="", S3="", S4="",
                            VALIDATE_RESULT=validate, LAUNCH_RESULT=launch)
            self.assertEqual(self.outputs()["reason"], reason)

    def context(self, data, state="ok"):
        (self.work / "outputs").unlink(missing_ok=True)
        (self.work / "fixture").write_text(data if isinstance(data, str) else json.dumps(data))
        self.stub("aws", '''[ "$1 $2" = 's3 cp' ] || exit 99
cat "$HOME/fixture" > "$4"''')
        return self.run_script("fetch-result-context.sh", STATE=state, RUN_ID="1-2", BUCKET="b",
                               RESULT_KEY="runs/1/2/campaign/result.json")

    def test_context_current_stale_and_malformed(self):
        good = {"schemaVersion": 1, "runId": "1-2", "benchRunId": "bench",
                "resultsUri": "s3://b/bench", "tarballKey": "runs/1/2/bench.tgz", "benchmarksSha": "a" * 40}
        self.assertEqual(self.context(good).returncode, 0)
        self.assertEqual(self.outputs()["benchmarks_sha"], "a" * 40)
        for data in ("broken json", good | {"runId": "1-1"}, good | {"schemaVersion": 2},
                     good | {"tarballKey": "a\nother=value"}, good | {"resultsUri": []}):
            self.assertEqual(self.context(data).returncode, 0)
            self.assertFalse((self.work / "outputs").exists())

    def test_excerpt_delimiter_and_attempt(self):
        data = {"schemaVersion": 1, "runId": "1-2", "verdict": "fail",
                "markdown": "first\nVERDICT_EXCERPT_EOF\nlast"}
        self.assertEqual(self.context(data, "fail").returncode, 0)
        lines = (self.work / "outputs").read_text().splitlines()
        self.assertEqual(lines[0], "excerpt<<VERDICT_EXCERPT_EOF_")
        self.assertEqual(lines[-1], "VERDICT_EXCERPT_EOF_")
        self.assertEqual(self.context(data | {"runId": "1-1"}, "fail").returncode, 0)
        self.assertFalse((self.work / "outputs").exists())

    def ingest(self, outcome, ref="main", token="stub-token"):
        (self.work / "outputs").unlink(missing_ok=True)
        # Stub ingest creates only local fixtures. The fake Git never calls real Git.
        self.stub("fake-ingest", '''mkdir -p docs/runs
printf '{}' > docs/runs/bench.json
case "$OUTCOME" in
  smoke) printf 'SMOKE SUMMARY: 0 passed, 1 failed\\nFAIL [viewer]\\n'; exit 1 ;;
  converter) printf 'converter failed\\n'; exit 1 ;;
  missing-viewer) exit 0 ;;
  *) printf 'runs: add bench\\n'; [[ "$*" != *--push-main* ]] || printf 'viewer: https://site.invalid/?run=bench\\n' ;;
esac''')
        self.stub("git", '''if [ "$1" = clone ]; then
  [ "$OUTCOME" != clone ] || exit 1
  mkdir -p "$3/scripts"
  # Copy the local stub as executable script using Bash, not a real checkout.
  cat "$HOME/bin/fake-ingest" > "$3/scripts/ingest.sh"
  chmod +x "$3/scripts/ingest.sh"
elif [ "$1" = -C ]; then
  case "$3" in
    checkout) [ "$OUTCOME" != checkout ] || exit 1 ;;
    config) [ "$OUTCOME" != config ] || exit 1 ;;
    rev-parse) printf 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\\n' ;;
    *) exit 99 ;;
  esac
else exit 99
fi''')
        # chmod is the only additional tool the clone fixture needs.
        import shutil
        if not (self.bin / "chmod").exists():
            chmod = shutil.which("chmod")
            assert chmod is not None, "required test tool missing: chmod"
            (self.bin / "chmod").symlink_to(chmod)
        self.stub("aws", '''[ "$1 $2" = 's3 cp' ] || exit 99
[ "$OUTCOME" != download ] || exit 1
printf 'stub archive' > "$4"''')
        return self.run_script("ingest-results-site.sh", OUTCOME=outcome, PUSH_TOKEN=token, BENCH_REPO_REF=ref,
                               BUCKET="b", TARBALL_KEY="bench.tgz", RESULTS_URI="s3://b/bench")

    def test_ingest_failures_emit_state_and_exit_zero(self):
        for outcome in ("download", "clone", "checkout", "config", "smoke", "converter", "missing-viewer"):
            with self.subTest(outcome=outcome):
                result = self.ingest(outcome)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(self.outputs()["ingest_state"], "failed")
                self.assertTrue(self.outputs()["ingest_reason"])
                self.assertNotIn("viewer_url", self.outputs())

    def test_ingest_success_and_skips(self):
        self.assertEqual(self.ingest("success").returncode, 0)
        self.assertEqual(self.outputs()["ingest_state"], "ingested")
        self.assertEqual(self.outputs()["viewer_url"], "https://site.invalid/?run=bench")
        self.assertEqual(self.ingest("success", ref="feature").returncode, 0)
        self.assertEqual(self.outputs()["ingest_state"], "skipped")
        self.assertNotIn("viewer_url", self.outputs())
        self.assertEqual(self.ingest("success", token="").returncode, 0)
        self.assertEqual(self.outputs()["ingest_state"], "skipped")

    def test_notification_survives_context_step_failure(self):
        workflow = (ROOT / ".github/workflows/bench-campaign.yml").read_text()
        self.assertIn("- name: Post the campaign notification\n        if: ${{ !cancelled() && steps.verdict.outputs.state != '' }}", workflow)
