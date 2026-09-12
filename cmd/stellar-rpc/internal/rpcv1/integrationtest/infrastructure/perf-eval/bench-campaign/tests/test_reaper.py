import json
import subprocess
import textwrap

from support import ROOT, ShellTest


class ReaperTest(ShellTest):
    def reap(self, rows, failure=""):
        workflow = (ROOT / ".github/workflows/bench-reaper.yml").read_text()
        # Execute the actual workflow step, not a copy of its selection logic.
        body = textwrap.dedent(workflow.split("        run: |\n", 1)[1].split("\n      # A reap", 1)[0])
        (self.work / "boxes.json").write_text(json.dumps(rows))
        self.stub("date", "printf '10000\\n'")
        self.stub("aws", '''case "$1 $2" in
  'ec2 describe-instances')
    [[ "$*" == *'Name=tag:test,Values=stellar-rpc-ci-load-test'* ]] || exit 99
    [[ "$*" == *'Name=tag:rpc-bench,Values=true'* ]] || exit 99
    [[ "$*" != *--no-paginate* ]] || exit 99
    [ "${FAILURE:-}" != describe ] || exit 1
    cat "$HOME/boxes.json" ;;
  'ec2 terminate-instances')
    printf '%s\\n' "$*" > "$HOME/terminated"
    [ "${FAILURE:-}" != terminate ] || exit 1 ;;
  *) exit 99 ;;
esac''')
        return subprocess.run([str(self.bin / "bash"), "-euo", "pipefail", "-c", body],
                              env=self.env | {"FAILURE": failure}, capture_output=True, text=True, timeout=5)

    def test_deadline_selection(self):
        result = self.reap([
            {"id": "i-expired", "deadline": "9999", "runId": "42"},
            {"id": "i-equal", "deadline": "10000"},
            {"id": "i-future", "deadline": "10001"},
            {"id": "i-missing", "deadline": None},
            {"id": "i-malformed", "deadline": "tomorrow"},
        ])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.work / "terminated").read_text(), "ec2 terminate-instances --instance-ids i-expired\n")
        self.assertEqual(self.outputs()["untagged"], "i-missing i-malformed")

    def test_no_expired_instances(self):
        self.assertEqual(self.reap([]).returncode, 0)
        self.assertFalse((self.work / "terminated").exists())

    def test_aws_errors_fail_the_job(self):
        for failure in ("describe", "terminate"):
            with self.subTest(failure=failure):
                self.assertNotEqual(self.reap([{"id": "i-1", "deadline": "1"}], failure).returncode, 0)
