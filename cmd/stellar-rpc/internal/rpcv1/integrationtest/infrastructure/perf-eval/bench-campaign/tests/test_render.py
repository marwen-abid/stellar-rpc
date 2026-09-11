import base64
import tomllib

from support import ShellTest


class RenderTest(ShellTest):
    def render(self, **env):
        (self.work / "outputs").unlink(missing_ok=True)
        return self.run_script("render-campaign-toml.sh", **(
            {"PHASE": "3", "INGEST": "hot", "HOT_NUM_LEDGERS": "200",
             "NOW_EPOCH": "1755000000"} | env))

    def test_config_and_deadline(self):
        result = self.render()
        self.assertEqual(result.returncode, 0, result.stderr)
        out = self.outputs()
        self.assertEqual(out["budget_minutes"], "126")
        self.assertEqual(out["deadline_epoch"], "1755007560")
        self.assertEqual(out["self_terminate_minutes"], "156")
        config = tomllib.loads(base64.b64decode(out["toml_b64"]).decode())
        self.assertEqual(config["runs"], 1)
        self.assertEqual(config["close_interval"], "600ms")
        self.assertEqual(config["query_duration"], "60s")
        self.assertEqual(len(config["dataset"]), 3)
        self.assertTrue(all(d["location"].endswith("/packs-v2/cold") for d in config["dataset"]))

    def test_invalid_inputs_produce_no_outputs(self):
        cases = {"PHASE": ["", "4"], "RUNS": ["0", "21", "08", "1e2", "9" * 40],
                 "WORKERS": ["129", "01", "9" * 40], "HOT_NUM_LEDGERS": ["10001", "08"],
                 "NOW_EPOCH": ["-1", "clock", "9" * 40], "CAPACITY_MINUTES": ["1261", "0"],
                 "SETUP_MARGIN_MINUTES": ["-1", "$(id)"],
                 "TARGET_REF": ["-main", 'a"', "a\nb"],
                 "BENCHMARKS_REF": ["$(id)", "a b"], "RUN_NAME": ["-x", "a/b"],
                 "PUBLISH_URI": ['s3://b/"\nx=1'], "INPUTS_PREFIX": ["s3://b/\\"]}
        for key, values in cases.items():
            for value in values:
                with self.subTest(key=key, value=value):
                    self.assertNotEqual(self.render(**{key: value}).returncode, 0)
                    self.assertFalse((self.work / "outputs").exists())

    def test_capacity_boundary(self):
        # 20 * 3 * 1900 ledgers * 0.6 seconds = 1140 minutes, plus 120 setup.
        self.assertEqual(self.render(RUNS="20", HOT_NUM_LEDGERS="1900").returncode, 0)
        self.assertEqual(self.outputs()["budget_minutes"], "1260")
        self.assertNotEqual(self.render(RUNS="20", HOT_NUM_LEDGERS="1901").returncode, 0)

    def test_query_budget_covers_six_txhash_rates(self):
        result = self.render(INGEST="both", QUERY="yes", QUERY_LEG_DURATION_SECONDS="120")
        self.assertEqual(result.returncode, 0, result.stderr)
        # Six dataset/tier pairs, each with 15 rate cells and four minutes of overhead.
        self.assertEqual(self.outputs()["budget_minutes"], "330")
        config = tomllib.loads(base64.b64decode(self.outputs()["toml_b64"]).decode())
        self.assertEqual(config["query_duration"], "120s")

    def test_empty_and_missing_query_databases(self):
        self.assertNotEqual(self.render(INGEST="none", QUERY="no").returncode, 0)
        for ingest in ("cold", "hot", "none"):
            self.assertNotEqual(self.render(INGEST=ingest, QUERY="yes").returncode, 0)
