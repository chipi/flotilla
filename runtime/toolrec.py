"""runtime/toolrec.py: a tool call as a run's calls file records it, with what it was called with kept short and clean.

A calls-file line's `tools` is a JSON array of {"name": "bash", "arg": "grep -n foo src/x.ts", "ok": true}: the tool's
name, its main argument (a command, a path, a pattern) cut at ARG_MAX characters after redaction, and whether the tool
worked (left out when the result is not known). flotillad keeps it in run_calls.tools (internal/store ParseTools).

The redaction is minimal and conservative, for what a tool argument can carry: API keys and tokens, bearer and basic
credentials, URL credentials, NAME=value for names that say key/token/secret/password, e-mail addresses, IPv4
addresses. It is not signal-fleet/mvp/scrub.py: that one also rewrites the operator's own identifiers for the public
eval corpus, and a worker image without signal-fleet cannot import it. bugfix/callrec_pi.py carries the same rules
(the bugfix worker image holds bugfix/ and not runtime/); bugfix/test_callrec_pi.py checks the two agree.
"""
import re

ARG_MAX = 160
MASK = "<redacted>"

_CREDENTIAL_RULES = [
    (re.compile(r"sk-or-v1-[A-Za-z0-9]{16,}|sk-[A-Za-z0-9_-]{20,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}"
                r"|glsa_[A-Za-z0-9_]{16,}|AKIA[0-9A-Z]{16}|xox[abprs]-[A-Za-z0-9-]{10,}"
                r"|eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+"), MASK),
    (re.compile(r"(?i)(authorization\s*[:=]\s*)(?:(?:bearer|basic|token)\s+)?[^\s'\"]+"), r"\1" + MASK),
    (re.compile(r"(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}"), r"\1 " + MASK),
    (re.compile(r"(://)[^/\s:@'\"]+:[^/\s@'\"]+@"), r"\1" + MASK + "@"),
    (re.compile(r"(?i)(--?(?:token|password|passwd|secret|api-?key)(?:=|\s+))(?:\"[^\"]*\"|'[^']*'|[^\s'\"]+)"), r"\1" + MASK),
    (re.compile(r"(?i)(\"[^\"]*(?:key|token|secret|passw(?:or)?d|credential)[^\"]*\"\s*:\s*)\"[^\"]*\""), r"\1\"" + MASK + "\""),
    (re.compile(r"(?i)\b([A-Za-z0-9_.-]*(?:key|token|secret|passw(?:or)?d|credential)[A-Za-z0-9_.-]*=)(?:\"[^\"]*\"|'[^']*'|[^\s'\"&;]+)"),
     r"\1" + MASK),
]
_RULES = _CREDENTIAL_RULES + [
    (re.compile(r"\b[A-Za-z0-9._%+-]+@[A-Za-z][A-Za-z0-9.-]*\.[A-Za-z]{2,}\b"), "<email>"),
    (re.compile(r"(?<![A-Za-z]/)\b\d{1,3}(?:\.\d{1,3}){3}\b"), "<ip>"),
]

# the argument a tool is best told by, first match wins: bash's command, grep's and find's pattern, then a path or a URL
_PREFERRED = ("command", "pattern", "path", "file_path", "filePath", "file", "url", "query")


def redact(text):
    for rx, repl in _RULES:
        text = rx.sub(repl, text)
    return text


def redact_credentials(text):
    """text with credentials masked (keys, tokens, bearer and basic, URL credentials, NAME=value for secret names):
    redact() without the e-mail and IPv4 rules, which are for a short argument in a record, not for a tool's result
    a model reads (the redact_credentials hook, runtime/hooks.py)."""
    for rx, repl in _CREDENTIAL_RULES:
        text = rx.sub(repl, text)
    return text


def clean_arg(text, n=ARG_MAX):
    """text redacted (before it is cut: a cut through a key would leave half of it), on one line, at most n characters."""
    text = " ".join(redact(str(text)).split())
    return text if len(text) <= n else text[:n - 1].rstrip() + "…"


def primary_arg(args):
    """The main argument of a tool call given as a dict (a pi toolCall's `arguments`, a probe's args): a preferred key's
    string, else the first string or number in the dict; "" when there is none."""
    if not isinstance(args, dict):
        return ""
    for k in _PREFERRED:
        if isinstance(args.get(k), str) and args[k]:
            return args[k]
    for v in args.values():
        if isinstance(v, str) and v:
            return v
        if isinstance(v, (int, float)) and not isinstance(v, bool):
            return str(v)
    return ""


def entry(name, arg="", ok=None):
    """One element of a line's `tools`; `arg` is cleaned here, `ok` is left out unless it is a bool."""
    out = {"name": str(name), "arg": clean_arg(arg) if arg else ""}
    if isinstance(ok, bool):
        out["ok"] = ok
    return out
