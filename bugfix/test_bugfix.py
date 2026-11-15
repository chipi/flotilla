"""python3 bugfix/test_bugfix.py: the bugfix fleet's routines (RFC-0010)."""
import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import pi_config  # noqa: E402

MODELS = {
    "endpoints": {
        "gateway": {"base_url": "http://litellm:4000/v1", "kind": "litellm", "key_env": "OPENROUTER_API_KEY"},
        "gateway-bugfix": {"base_url": "http://litellm:4000/v1", "kind": "litellm", "key_file": "/run/flotilla/model.key"},
    },
    "aliases": {
        "triage-flash": {"endpoint": "gateway", "model": "fleet-triage-flash"},
        "bugfix-flash": {"endpoint": "gateway-bugfix", "model": "fleet-bugfix-flash"},
        "bugfix-reviewer": {"endpoint": "gateway-bugfix", "model": "fleet-bugfix-reviewer"},
    },
}


class PiConfig(unittest.TestCase):
    def test_only_the_named_aliases_and_the_key_by_name(self):
        """T4: pi sees only the routine's aliases; the key is named (a file to cat), never written."""
        out = pi_config.models_json(MODELS, ["bugfix-flash", "bugfix-reviewer"])
        self.assertEqual(list(out["providers"]), ["flotilla-gateway-bugfix"])
        p = out["providers"]["flotilla-gateway-bugfix"]
        self.assertEqual((p["baseUrl"], p["api"], p["apiKey"]), ("http://litellm:4000/v1", "openai-completions", "!cat /run/flotilla/model.key"))
        self.assertEqual([m["id"] for m in p["models"]], ["fleet-bugfix-flash", "fleet-bugfix-reviewer"])
        self.assertNotIn("fleet-triage-flash", json.dumps(out))
        env = pi_config.models_json(MODELS, ["triage-flash"])["providers"]["flotilla-gateway"]
        self.assertEqual(env["apiKey"], "$OPENROUTER_API_KEY")
        self.assertEqual(pi_config.pick(MODELS, "bugfix-flash"), ("flotilla-gateway-bugfix", "fleet-bugfix-flash"))

    def test_unknown_alias_and_missing_section(self):
        with self.assertRaises(pi_config.ConfigError):
            pi_config.models_json(MODELS, ["nope"])
        with self.assertRaises(pi_config.ConfigError):
            pi_config.write(tempfile.mkdtemp(), ["bugfix-flash"], environ={})

    def test_written_private(self):
        d = tempfile.mkdtemp()
        pi_config.write(d, ["bugfix-flash"], environ={"FLOTILLA_MODELS": json.dumps(MODELS)})
        path = os.path.join(d, "models.json")
        self.assertEqual(os.stat(path).st_mode & 0o777, 0o600)
        with open(path) as f:
            self.assertIn("fleet-bugfix-flash", f.read())


if __name__ == "__main__":
    unittest.main()
