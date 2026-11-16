"""Action handlers + the append-only dispositions ledger (SIGNALS §7, §9, §10).

- dismiss: record (propose-first for MVP — no source ack yet). If it carries an
  immediate_recommendation, ALSO emit a `config-enhancement` File (dry-run) — the
  Tune dual output (SIGNALS §7.1 / review R3-3), recorded on the ledger row via the
  `followup` column (review R4-3).
- file:    build the labelled GitHub issue payload; DRY-RUN until the target repo
           is chosen (SIGNALS §13.1 #3).
- escalate: operator note.
Idempotency is keyed on the OCCURRENCE (fingerprint + startsAt), not the
alertname (review R3-1).
"""
import datetime
import hashlib
import json
import os

import capture
import config

LEDGER_COLS = ["ts", "occurrence_id", "fingerprint", "source", "alertname",
               "disposition", "work_type", "followup", "model", "prompt_ver",
               "prompt_sha", "gates", "n_probes", "certainty", "reason",
               "signal_count", "cycle_id", "stage"]


def _now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def _cell(s):
    return str(s).replace("\t", " ").replace("\n", " ")


def _ensure_ledger_schema():
    """Migrate-once on header mismatch so the overturn dataset never becomes a
    mixed-schema TSV (review R4-1). An old-schema file is set aside, not appended
    to under a stale header."""
    if not os.path.exists(config.LEDGER):
        return
    with open(config.LEDGER) as f:
        header = f.readline().rstrip("\n")
    if header != "\t".join(LEDGER_COLS):
        bak = f"{config.LEDGER}.{_now().replace(':', '').replace('.', '')}.bak"
        os.rename(config.LEDGER, bak)
        print(f"  [ledger] schema changed — archived old ledger to {os.path.basename(bak)}")


def ledger_append(signal, disp, followup=""):
    os.makedirs(os.path.dirname(config.LEDGER), exist_ok=True)
    _ensure_ledger_schema()
    new = not os.path.exists(config.LEDGER)
    meta = disp.get("_meta", {})
    wt = (disp.get("file") or {}).get("work_type", "") if disp.get("disposition") == "file" else ""
    row = [_now(), signal.get("occurrence_id", ""), signal.get("fingerprint", ""),
           signal.get("source", ""), signal.get("alertname", ""), disp["disposition"],
           wt, followup, meta.get("model", ""), meta.get("prompt_ver", ""),
           meta.get("prompt_sha", ""), meta.get("gates", ""), str(meta.get("n_probes", "")),
           meta.get("certainty", "") or "", disp.get("reason", "")[:300],
           str(signal.get("labels", {}).get("count", "")),
           os.environ.get("FLEETD_CYCLE_ID", ""),
           os.environ.get("FLEETD_STAGE", "")]
    cells = [_cell(x) for x in row]
    if os.environ.get("SF_LEDGER_BACKEND", "tsv") == "api":   # RFC-0002 §9 step 5: the store first, then the TSV
        import filing
        kind = "recurrence" if disp["disposition"] == "recurrence" else "decision"
        filing._ledger_client().append_entry(kind, dict(zip(LEDGER_COLS, cells)))
    with open(config.LEDGER, "a") as f:
        if new:
            f.write("\t".join(LEDGER_COLS) + "\n")
        f.write("\t".join(cells) + "\n")


def last_for_fingerprint(fp):
    """Most recent NON-recurrence ledger row for a fingerprint: the baseline a
    recurrence check compares against. Returns {ts, disposition, count,
    latest_count} or None — latest_count is the count on the newest row of ANY kind
    (recurrence rows included), i.e. the last count the ledger already recorded."""
    if not os.path.exists(config.LEDGER):
        return None
    fi = LEDGER_COLS.index("fingerprint")
    di = LEDGER_COLS.index("disposition")
    ti = LEDGER_COLS.index("ts")
    ci = LEDGER_COLS.index("signal_count")
    last, latest_count = None, 0
    with open(config.LEDGER) as f:
        for n, line in enumerate(f):
            if n == 0:
                continue
            p = line.rstrip("\n").split("\t")
            if len(p) <= fi or p[fi] != fp:
                continue
            cnt = int(p[ci]) if len(p) > ci and p[ci].isdigit() else 0
            latest_count = cnt
            if len(p) <= di or p[di] != "recurrence":
                last = {"ts": p[ti], "disposition": p[di] if len(p) > di else "",
                        "count": cnt}
    if last:
        last["latest_count"] = latest_count
    return last


def decisions_for_fingerprint(fp):
    """Every NON-recurrence ledger row for a fingerprint, oldest first, as {ts, disposition}."""
    if not os.path.exists(config.LEDGER):
        return []
    fi = LEDGER_COLS.index("fingerprint")
    di = LEDGER_COLS.index("disposition")
    ti = LEDGER_COLS.index("ts")
    out = []
    with open(config.LEDGER) as f:
        for n, line in enumerate(f):
            if n == 0:
                continue
            p = line.rstrip("\n").split("\t")
            if len(p) <= di or p[fi] != fp or p[di] == "recurrence":
                continue
            out.append({"ts": p[ti], "disposition": p[di]})
    return out


def record_recurrence(signal, prior):
    """A fingerprint re-fired after a disposition: log it (the implicit-overturn
    signal for Dismiss, review R2-2) without paying for a fresh triage."""
    disp = {"disposition": "recurrence",
            "reason": f"recurred after {prior['disposition']} @ {prior['ts']} "
                      f"(count {prior['count']} -> {signal.get('labels', {}).get('count')})",
            "_meta": {}}
    print(f"[RECURRENCE] {signal.get('fingerprint')} after {prior['disposition']} "
          f"({prior['count']} -> {signal.get('labels', {}).get('count')})")
    ledger_append(signal, disp)
    import observ  # local import — avoids a cycle at module load
    observ.push_disposition_metric(signal, disp)


def already_done(occurrence_id):
    """Last disposition for this OCCURRENCE, or None (keyed on occurrence_id so a
    correct Dismiss of one firing never silences the next firing — review R3-1)."""
    if not os.path.exists(config.LEDGER):
        return None
    oi, di = LEDGER_COLS.index("occurrence_id"), LEDGER_COLS.index("disposition")
    last = None
    with open(config.LEDGER) as f:
        for n, line in enumerate(f):
            if n == 0:
                continue  # header
            p = line.rstrip("\n").split("\t")
            if len(p) > max(oi, di) and p[oi] == occurrence_id:
                last = p[di]
    return last


def signal_code_version(signal):
    """#6 — the release/commit the signal fired on, so a reader (and the fix fleet)
    resolves stack frames against the right revision. GlitchTip carries it on the
    issue's last/first release; '' when unknown."""
    raw = signal.get("raw") or {}
    for key in ("lastRelease", "firstRelease"):
        rel = raw.get(key) or {}
        v = rel.get("shortVersion") or rel.get("version")
        if v:
            return v
    return ""


def parse_utc(ts):
    """An ISO timestamp as an aware UTC datetime; None when absent or unparsable. No offset reads as UTC."""
    # fractions dropped first: python 3.9's fromisoformat (the mini's) refuses GlitchTip's 1-5 digit ones
    v = str(ts or "").replace("Z", "+00:00")
    if "." in v:
        head, _, rest = v.partition(".")
        v = head + rest[len(rest) - len(rest.lstrip("0123456789")):]
    try:
        t = datetime.datetime.fromisoformat(v)
    except ValueError:
        return None
    return (t if t.tzinfo else t.replace(tzinfo=datetime.timezone.utc)).astimezone(datetime.timezone.utc)


def _utc(ts):
    """'2026-11-14T08:01:02.5Z' -> '2026-11-14 08:01 UTC'; '' when absent or unparsable."""
    t = parse_utc(ts)
    return t.strftime("%Y-%m-%d %H:%M UTC") if t else ""


def _span_safe(value, cap=200):
    """A GlitchTip value (SDK-supplied, so untrusted) made safe for one line of a public issue: whitespace
    collapsed, backticks removed so it cannot leave its code span, a zero-width space after every '@' so it
    cannot ping a team, and capped."""
    v = " ".join(str(value).replace("`", "").split()).replace("@", "@\u200b")
    return v if len(v) <= cap else v[:cap - 1] + "\u2026"


def _safe_url(url):
    u = str(url or "")
    ok = u.startswith(("https://", "http://")) and not any(c.isspace() or c in ")>" for c in u)
    return u if ok else ""


def signal_facts(signal):
    """Lines stating what the polled GlitchTip issue already holds: a link to it, where it was thrown, how often and
    since when (ROADMAP T0.7 Q3; the issue-quality rubric's R2, R3, R5). None of it comes from the model."""
    raw = signal.get("raw") or {}
    out = []
    link = _safe_url(raw.get("permalink"))
    if link:
        out.append(f"**Source:** [{_span_safe(raw.get('shortId') or 'GlitchTip issue')}]({link})")
    culprit = _span_safe(raw.get("culprit") or "")
    if culprit:
        out.append(f"**Where:** `{culprit}`")
    first, last = _utc(raw.get("firstSeen")), _utc(raw.get("lastSeen"))
    if raw.get("count") not in (None, "") and first:
        users = raw.get("userCount")
        seen = f"{raw['count']} events" + (f", {users} users" if users not in (None, "") else "")
        out.append(f"**Seen:** {seen}; first {first}" + (f", last {last}" if last else ""))
    ev = signal.get("event_facts") or {}
    rel = _span_safe(ev.get("release") or "") if not signal_code_version(signal) else ""   # the Code version line says it otherwise
    env = _span_safe(ev.get("environment") or "")
    parts = [p for p in (env, f"release `{rel}`" if rel else "") if p]
    if parts:
        out.append("**Environment:** " + ", ".join(parts))
    return out


def event_facts(signal, disp, fetch=False):
    """Environment and release from the error's latest event (T0.7 Q3b, rubric R4): the event_detail probe's
    result when the model fetched it, else, only when `fetch` (a real filing, never a dry run or a replay), one
    best-effort read of the latest event. {} when neither gives anything."""
    s = next((t.get("result") for t in disp.get("_probe_trace") or []
              if t.get("probe") == "event_detail" and isinstance(t.get("result"), dict)), None)
    if s is None and fetch and signal.get("source") == "glitchtip" and signal.get("issue_id"):
        try:
            import correlate
            s = correlate._event_summary(signal["issue_id"])
        except Exception as e:  # noqa: BLE001 -- the facts are a convenience; filing must not fail on them
            print(f"  [event facts unavailable] {type(e).__name__}")
            s = None
    if not isinstance(s, dict):
        return {}
    facts = {"environment": s.get("environment") or "", "release": s.get("code_version") or s.get("release") or ""}
    return {k: v for k, v in facts.items() if v}


def build_issue(signal, f):
    """GitHub issue payload for a File-shaped item. Label = work_type (the router)."""
    body = [
        f"**Signal:** `{signal.get('alertname')}` "
        f"(source: {signal.get('source')}, fp: `{signal.get('fingerprint')}`)",
    ]
    body += signal_facts(signal)
    cv = signal_code_version(signal)
    if cv:
        body.append(f"**Code version:** `{cv}`  _(frames resolve against this revision)_")
    body += [
        "", f"**Symptom:** {f.get('symptom', '')}",
        "", f"**Area:** {f.get('area', '')}",
        "", "**Acceptance criteria (intent-cited):**",
    ]
    for c in f.get("acceptance", []):
        body.append(f"- {c.get('criterion')}  _(intent: {c.get('intent_source')})_")
    body.append("")
    body.append("**Evidence:**")
    for e in f.get("evidence", []):
        body.append(f"- {e}")
    body.append("")
    body.append("_Filed by signal-fleet MVP._")
    labels = [f.get("work_type", "config-enhancement")]
    area = (f.get("area") or "").strip().lower()
    if area:                       # #8 — area label for release-planning filters
        labels.append(f"area:{area}")
    return {"title": f.get("title", ""), "labels": labels, "body": "\n".join(body)}


def _tune_followup(signal, disp):
    """The File half of the Tune dual output: a config-enhancement built from the
    Dismiss's immediate_recommendation (SIGNALS §7.1). None if no recommendation.

    intent_source is `triager-recommendation` — the triager invented it, no
    operator stated it, so it must NOT masquerade as `operator-rule` (review R4-2).
    This sub-bar source is acceptable ONLY because config-enhancement is
    operator-gated; it is never valid for a `bug` File (which the intent gate
    enforces — `triager-recommendation` is deliberately not in ALLOWED_INTENT)."""
    rec = disp.get("immediate_recommendation")
    if not rec:
        return None
    return {
        "work_type": "config-enhancement",
        "title": f"[config] tune alert: {signal.get('alertname', '')}",
        "symptom": f"Dismissed as false alarm; recurring noise: {disp.get('reason', '')[:180]}",
        "area": "monitoring-config",
        "evidence": [f"signal fingerprint {signal.get('fingerprint')}"],
        "acceptance": [{"criterion": rec, "intent_source": "triager-recommendation"}],
    }


def queue_proposal(signal, issue, kind):
    """Write a File / config-enhancement proposal as a DRAFT to the queue for
    operator review (propose-first — EVAL.md transition (i)). Not a real issue;
    real issue creation is gated on the File-quality eval (transition (iii))."""
    os.makedirs(config.QUEUE_DIR, exist_ok=True)
    # dedup slug prefers the normalized key (#1) so the SHADOW path collapses the
    # same fragmentation the live path does (operator 2026-08-28: extend to shadow)
    # — else 10 volatile-fingerprint occurrences queue 10 drafts. Falls back to the
    # fingerprint when there's nothing stable to normalize.
    import filing
    nk = filing.normalized_key(signal)
    slug = (nk or signal.get("fingerprint") or "sig").replace(":", "_").replace("/", "_")
    # one queued proposal per (kind, slug) — titles drift per day, so hashing the
    # title alone let duplicates pile up (2026-08-05: 42 config-enhancement drafts,
    # mostly re-proposals of the same tunes)
    import glob as _glob
    existing = _glob.glob(os.path.join(config.QUEUE_DIR, f"{kind}-{slug}-*.json"))
    if existing:
        return existing[0]
    h = hashlib.sha1((issue.get("title", "")).encode()).hexdigest()[:6]
    path = os.path.join(config.QUEUE_DIR, f"{kind}-{slug}-{h}.json")
    with open(path, "w") as f:
        json.dump({"queued_at": _now(), "kind": kind,
                   "signal": {k: signal.get(k) for k in
                              ("fingerprint", "occurrence_id", "source", "alertname")},
                   "issue": issue}, f, indent=2)
    return path


def act(signal, disp, dry_run=True):
    d = disp["disposition"]
    followup = ""
    if d == "dismiss":
        print(f"[DISMISS] {disp.get('reason', '')[:140]}")
        follow = _tune_followup(signal, disp)
        if follow:
            followup = "config-enhancement"
            p = queue_proposal(signal, build_issue(signal, follow), "config-enhancement")
            print(f"  [+ queued config-enhancement proposal] {os.path.basename(p)}")
    elif d == "cleanup":
        prop = {"title": f"cleanup: {signal.get('alertname', '')}",
                "action": "resolve+tag (NOT delete)", "target": signal.get("fingerprint"),
                "marker": disp.get("marker"), "reason": disp.get("reason", "")[:200]}
        p = queue_proposal(signal, prop, "cleanup")
        print(f"[CLEANUP queued] resolve+tag {signal.get('fingerprint')} (marker: {disp.get('marker')}) "
              f"-> {os.path.basename(p)}")
    elif d == "file":
        signal = dict(signal, event_facts=event_facts(signal, disp, fetch=not dry_run))
        if dry_run:
            p = queue_proposal(signal, build_issue(signal, disp["file"]),
                               disp["file"].get("work_type", "bug"))
            print(f"[FILE queued proposal] {os.path.basename(p)}")
        else:
            # propose stage: real GH issue via the filing tree (fingerprint
            # ledger dedup, storm grouping, reopen-vs-regression, mute honor)
            import filing
            out = filing.file_or_update(signal, build_issue(signal, disp["file"]),
                                        disp["file"].get("work_type", "bug"))
            print(f"[FILE] {out}")
    elif d == "escalate":
        q = disp.get("question", "")
        print(f"[ESCALATE -> operator] {disp.get('reason', '')[:130]}"
              + (f"  Q: {q[:110]}" if q else ""))
        if not dry_run:
            # propose stage: escalation lands as a GH issue whose GitHub-native
            # actions (close / route-label / comment / mute) ARE the operator UI
            import filing
            print(f"  [ESCALATE] {filing.file_escalation(signal, disp)}")
    ledger_append(signal, disp, followup=followup)
    capture.record(signal, disp)
