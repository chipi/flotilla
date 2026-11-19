"""bugfix/pi_review: pi's review call as the chain has always made it.

The platform's pi adapter (runtime/harness_pi.py) serves any station and any `tools` value on its own configuration:
a models.json it writes, the prompt on stdin. The chain's review keeps the call it always sent: the provider the chain
configured under PI_CODING_AGENT_DIR (bugfix/pi_config.py), the prompt as the last argument, the chain's own environment
and no extra phone-home variables (the worker image sets them). bugfix/test_pi_golden.py pins that argv byte for byte;
moving the review onto the generic adapter is a behaviour change for the installation's own replay (v2-design.md section
7, step 6), not part of building the layer.
"""
import os
import sys

sys.path.append(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "runtime"))
import adapter  # noqa: E402
import harness_pi  # noqa: E402


class ChainReview(harness_pi.Pi):
    prompt_via_stdin = False   # one argv string: Chain.review keeps it under Linux's 128 KiB (REVIEW_PROMPT_MAX)

    def build(self, req):
        provider = req.model.meta.get("pi_provider") or "flotilla-" + req.model.endpoint
        rules = ["--no-context-files", "--append-system-prompt", req.system_path] if req.system_path else []
        if req.tools == "none" and rules:   # the rules are about using tools; a review without them has none
            rules = ["--no-context-files"]
        tools = {"none": ["--no-tools"], "read": ["--tools", harness_pi.READ_ONLY_TOOLS]}.get(req.tools, [])
        return adapter.Invocation(argv=["pi", "-p", "--mode", "json", "--provider", provider, "--model", req.model.model,
                                        *tools, *rules, req.prompt], cwd=req.workdir)

    def phone_home_env(self):
        return {}
