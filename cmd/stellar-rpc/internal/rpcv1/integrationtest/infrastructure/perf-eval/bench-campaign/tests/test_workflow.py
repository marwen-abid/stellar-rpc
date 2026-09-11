from support import ROOT, ShellTest


class WorkflowTest(ShellTest):
    def test_gate_table(self):
        # Avoid reading a developer's /tmp reports on failure paths.
        self.stub("cat", "exit 1")
        for state in ("ok", "fail", "running", "", "bogus"):
            for next_window in ("", "window 2/4"):
                with self.subTest(state=state, next_window=next_window):
                    result = self.run_script("gate-relay-state.sh", STATE=state, NEXT=next_window)
                    passed = state == "ok" or (state == "running" and bool(next_window))
                    self.assertEqual(result.returncode == 0, passed, result.stdout)

    def test_launch_tags_deadline_and_data(self):
        self.stub("date", "printf '10000\\n'")
        self.stub("aws", '''[ "$1 $2" = 'ec2 run-instances' ] || exit 99
printf '%s\\n' "$@" > "$HOME/launch-args"
printf '{"Instances":[{"InstanceId":"i-123"}]}\\n'
''')
        result = self.run_script("launch-box.sh", INSTANCE_TYPE="m6id.2xlarge", CAMPAIGN_NAME="test",
                                 SELF_TERMINATE_MINUTES="150", RUN_ID_TAG="42",
                                 USER_DATA=str(self.work / "user data.gz"))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.outputs()["instance_id"], "i-123")
        args = (self.work / "launch-args").read_text()
        for tag in ("{Key=test,Value=stellar-rpc-ci-load-test}", "{Key=rpc-bench,Value=true}",
                    "{Key=deadline,Value=20800}", "{Key=run-id,Value=42}"):
            self.assertEqual(args.count(tag), 2)
        self.assertIn("fileb://" + str(self.work / "user data.gz") + "\n", args)

    def test_attempt_identity_and_windows(self):
        workflow = (ROOT / ".github/workflows/bench-campaign.yml").read_text()
        self.assertIn("RESULT_KEY: runs/${{ github.run_id }}/${{ github.run_attempt }}/campaign/result.json", workflow)
        self.assertLess(workflow.index("name: Seed the pending result marker"), workflow.index("name: Launch EC2 instance"))
        self.assertEqual(workflow.count("uses: ./.github/actions/bench-poll"), 4)
        self.assertEqual(workflow.count("timeout-minutes: 350"), 4)
        for n in (1, 2, 3):
            self.assertIn(f"if: needs.poll{n}.outputs.state == 'running'", workflow)
        action = (ROOT / ".github/actions/bench-poll/action.yml").read_text()
        self.assertIn("WINDOW_SECONDS: 19200", action)
        self.assertIn("go run", action)
        self.assertNotIn("uses: ./.github/actions/setup-go", action)
