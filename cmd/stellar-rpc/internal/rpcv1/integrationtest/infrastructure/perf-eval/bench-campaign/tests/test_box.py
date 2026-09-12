import base64
import gzip
import random
import subprocess

from support import CAMPAIGN, ShellTest


class BoxTest(ShellTest):
    def test_user_data_quotes_and_compresses(self):
        values = {"RUN_ID": "1-2", "BUCKET": "test-bucket", "RESULT_KEY": "runs/1/2/result.json",
                  "SELF_TERMINATE_MINUTES": "156", "BUDGET_MINUTES": "126",
                  "BENCH_TOML_B64": "YQ==", "BENCH_REPO_REF": 'branch\"; $(exit 9)\nname'}
        out = self.work / "user-data.sh"
        result = self.run_script("render-user-data.sh", OUT=str(out), **values)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(gzip.decompress(out.with_suffix(".sh.gz").read_bytes()), out.read_bytes())
        self.assertLessEqual(out.with_suffix(".sh.gz").stat().st_size, 16384)
        # Execute only the six-line export preamble, never bootstrap or the leg.
        preamble = "\n".join(out.read_text().splitlines()[:6])
        result = subprocess.run([str(self.bin / "bash"), "-c", preamble + '\nprintf "%s" "$BENCH_REPO_REF"'],
                                env=self.env, text=True, capture_output=True, timeout=5)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, values["BENCH_REPO_REF"])

    def test_user_data_rejects_oversize(self):
        payload = base64.b64encode(random.Random(1).randbytes(20000)).decode()
        result = self.run_script("render-user-data.sh", OUT=str(self.work / "ud"),
                                 RUN_ID="1-1", BUCKET="b", RESULT_KEY="r", SELF_TERMINATE_MINUTES="150",
                                 BUDGET_MINUTES="120", BENCH_TOML_B64=payload, BENCH_REPO_REF="main")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("16384-byte EC2 limit", result.stdout)

    def test_verdict_survives_shutdown_failure(self):
        self.stub("go", "printf 'published: s3://results/run-one\\n'")
        self.stub("poweroff", "exit 1")
        source = (CAMPAIGN / "run-campaign.sh").read_text()
        # Run the real verdict branch with an isolated filesystem and fake helpers.
        body = source[source.index('log "running campaign"'):]
        body = body.replace("/root/stellar-rpc-benchmarks/runner", str(self.work))
        body = body.replace("/tmp/", str(self.work) + "/")
        prefix = '''set -euo pipefail
log() { :; }
upload_bundle() { TARBALL_KEY=bundle; }
upload_box_log() { :; }
upload_result() { printf '%s\\n' "$1" >> "$HOME/verdicts"; }
bail() { upload_result fail; exit 1; }
trap 'bail' ERR
'''
        result = subprocess.run([str(self.bin / "bash"), "-c", prefix + body],
                                env=self.env | {"LEG_TITLE": "Campaign", "RUN_ID": "1-1",
                                                "BENCH_REPO_REF": "main", "RESULTS_FILE": str(self.work / "result.md")},
                                text=True, capture_output=True, timeout=5)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.work / "verdicts").read_text(), "ok\n")

    def test_failed_bundle_upload_clears_sidecar_key(self):
        self.stub("aws", '''case "$*" in
  *application/gzip*) exit 1 ;;
  *application/json*) exit 0 ;;
  *) exit 99 ;;
esac''')
        source = (CAMPAIGN / "run-campaign.sh").read_text()
        body = source[source.index("upload_bundle() {"):source.index("# newest_bundle_tarball")]
        body = body.replace("/tmp/", str(self.work) + "/")
        result = subprocess.run([str(self.bin / "bash"), "-c",
                                 'set -euo pipefail\nlog() { :; }\n' + body + '\nupload_bundle missing.tgz bench s3://results/bench'],
                                env=self.env | {"BUCKET": "b", "RESULT_KEY": "runs/1/2/result.json",
                                                "RUN_ID": "1-2", "BENCH_REPO_SHA": "a" * 40},
                                text=True, capture_output=True, timeout=5)
        self.assertEqual(result.returncode, 0, result.stderr)
        import json
        sidecar = json.loads((self.work / "run-info.json").read_text())
        self.assertEqual(sidecar["tarballKey"], "")
        self.assertEqual(sidecar["runId"], "1-2")
        self.assertEqual(sidecar["benchmarksSha"], "a" * 40)
