"""bugfix/harness_prompts.py: the chain's fix and reproducer prompts as Python, for the harnesses that are not pi.

pi gets them from bash: harnesses/pi.sh renders the fix prompt (a heredoc around the task) and harnesses/repro.sh the
reproducer's (agents/repro.md's body, then the issue). A harness reached through runtime/adapter.py is given the prompt
in the request, so the same text is built here. bugfix/test_harness_prompts.py renders the shell versions with bash and
compares, as test_declared_prompts.py keeps declared_prompts.py in agreement with runtime/prompts.py: change one side,
change the other.

What bash does that this reproduces on purpose: `read -r -d '' PROMPT <<EOF` drops the whitespace (space, tab, newline)
at both ends of the text, `$(awk ...)` drops the trailing newlines of the agent definition, and awk's `/^---$/` takes a
line of exactly three dashes, wherever it is, for a front-matter fence and prints none of them.
"""

FIX_HEAD = "You are fixing a bug in the repository at the current working directory.\n\nBug report:\n"
FIX_TAIL = ("\n\nDiagnose the cause in the source and fix it. Add or update a regression test if\n"
            "appropriate. Keep the change tight and correct; do not touch unrelated code.\n"
            "When done, stop — do not commit, push, or open a PR.")


def _read_trim(text):
    """What `read -r -d ''` keeps: the text without the IFS whitespace at either end."""
    return text.strip(" \t\n")


def fix_prompt(desc):
    """harnesses/pi.sh's prompt for a fresh fix session on `desc` (the task and the chain's note). A resumed session is
    sent `desc` itself, with no template: the chain passes it as the request's prompt."""
    return _read_trim(FIX_HEAD + desc + FIX_TAIL)


def agent_def_body(text):
    """repro.sh's `awk '/^---$/{c++;next} c>=2'` over an agent definition, in `$(...)`: the lines after the second
    line of exactly `---`, each with its newline, less the trailing newlines."""
    lines = text.split("\n")
    if lines and lines[-1] == "":
        lines.pop()
    out, fences = [], 0
    for ln in lines:
        if ln == "---":
            fences += 1
        elif fences >= 2:
            out.append(ln)
    return "\n".join(out).rstrip("\n")


def repro_prompt(agent_def, desc):
    """repro.sh's prompt: the agent definition's body (agent_def_body of agents/repro.md), a blank line, then `desc`, the
    issue as filed and the verdict's task."""
    return _read_trim(agent_def + "\n\n" + desc)
