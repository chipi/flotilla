#!/usr/bin/env python3
"""Mirror the triage fleet's state out of its volume into a host folder, between runs (phase-2-plan,
agreed with the fleet session 2026-11-08). Host-side, every 2 minutes from launchd; stdlib only.

  mirror.py [--dest DIR] [--once-verbose]

What it copies: results/ (dispositions.tsv, filed.tsv, probes/, ...), queue/ and export/, from the
volume named by TRIAGE_STATE_VOLUME (FLOTILLA_HOME/.env). Never frozen/ (the weekly freeze writes it
on the host), never any env file, never anything outside those three folders.

Only between runs, and checked twice: the copy is made only when no triage run is in flight and the
last run ended after the stamp, and it is kept only if, read again after the copy, no run is in
flight and no newer run has started. Otherwise it is thrown away and the next tick tries again. Each
file lands whole: written next to its destination, then renamed. queue/ and export/ are synced (a
file gone from the volume goes from the mirror too); results/ only gains and updates.

Writes, besides the copies:
  results/.mirror_stamp  {"end": <UTC end of the last run included>, "run_id": ..., "copied_at": ...}
  results/cycles.tsv     run_id, start_utc, end_utc, status, stage: one row per triage run

Into the live fleet folder (~/signal-fleet) only after the switch: it refuses unless MIRROR_LIVE=1 is
in FLOTILLA_HOME/.env, fleetd's STOP flag is in that folder, and the triage job is not in shadow.
Ends with one line: MIRROR PASS / SKIP (why) / RACED / FAIL.
"""
import argparse
import io
import json
import os
import subprocess
import sys
import tarfile
import tempfile
import time

H = os.environ.get("FLOTILLA_HOME", os.path.expanduser("~/flotilla"))
LIVE = os.path.realpath(os.path.expanduser("~/signal-fleet"))
JOB = "triage/cycle"
TREES = ("results", "queue", "export")
SYNCED = ("queue", "export")          # deletions follow the volume here; results/ only gains
# written on the host, not by the fleet: the deploy log (homelab's deploy hooks, then flotilla's
# deploy append it there). The volume's copy is stale, so the mirror never writes it.
HOST_OWNED = {"results/deploys.tsv"}
DONE = ("ok", "error", "timeout", "interrupted")


def env_file(path):
    out = {}
    try:
        for line in open(path):
            line = line.strip()
            if line and not line.startswith("#") and "=" in line:
                k, v = line.split("=", 1)
                out[k.strip()] = v.strip()
    except OSError:
        pass
    return out


def docker(*args, stdin=None):
    return subprocess.run(["docker", *args], capture_output=True, check=True, input=stdin)


def state():
    """flotillad's view: is the job running, and its runs (newest first)."""
    st = json.loads(docker("exec", "flotillad", "/usr/local/bin/flotilla", "status", "-json").stdout)
    runs = json.loads(docker("exec", "flotillad", "/usr/local/bin/flotilla", "runs", JOB, "-n", "200", "-json").stdout)["runs"]
    job = next((j for j in st["jobs"] if j["id"] == JOB), None)
    if job is None:
        raise RuntimeError(f"flotillad has no job {JOB}")
    return {"running": bool(job.get("running")), "stage": job["stage"], "runs": runs or []}


def copy_out(volume, image, dest_tmp):
    """The three trees out of the volume, read-only, by a throwaway container with no network."""
    sh = ("cd /s && set -- && for d in " + " ".join(TREES) + "; do [ -d \"$d\" ] && set -- \"$@\" \"$d\"; done; "
          "[ $# -gt 0 ] && tar --exclude='*.env' --exclude='*.env.*' -cf - \"$@\"")
    out = docker("run", "--rm", "--network", "none", "-v", f"{volume}:/s:ro", "--entrypoint", "sh", image, "-c", sh).stdout
    with tarfile.open(fileobj=io.BytesIO(out)) as t:
        for m in t.getmembers():
            if not (m.isfile() or m.isdir()) or m.name.startswith("/") or ".." in m.name.split("/"):
                raise RuntimeError(f"refusing tar member {m.name!r}")
        t.extractall(dest_tmp)


def newest_done(runs):
    done = [r for r in runs if r.get("status") in DONE and r.get("ended_at")]
    return max(done, key=lambda r: r["ended_at"]) if done else None


def newest_start(runs):
    real = [r for r in runs if r.get("status") != "skipped"]
    return max((r["started_at"] for r in real), default="")


def write_atomic(path, data):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path), prefix=".mirror-")
    with os.fdopen(fd, "wb") as f:
        f.write(data)
    os.chmod(tmp, 0o644)
    os.replace(tmp, path)


def install(src_root, dest):
    """Copy every file of src_root into dest atomically; sync deletions in SYNCED trees only."""
    changed = removed = 0
    seen = set()
    for top in TREES:
        base = os.path.join(src_root, top)
        for dirpath, _, files in os.walk(base):
            for name in files:
                rel = os.path.relpath(os.path.join(dirpath, name), src_root)
                if rel in HOST_OWNED:
                    continue
                seen.add(rel)
                data = open(os.path.join(src_root, rel), "rb").read()
                target = os.path.join(dest, rel)
                try:
                    if open(target, "rb").read() == data:
                        continue
                except OSError:
                    pass
                write_atomic(target, data)
                changed += 1
    for top in SYNCED:
        for dirpath, _, files in os.walk(os.path.join(dest, top)):
            for name in files:
                rel = os.path.relpath(os.path.join(dirpath, name), dest)
                if rel not in seen and not name.startswith(".mirror-"):
                    os.remove(os.path.join(dest, rel))
                    removed += 1
    return changed, removed


def cycles_rows(runs, after):
    rows = sorted((r for r in runs if r.get("status") in DONE and r.get("ended_at") and r["ended_at"] > after),
                  key=lambda r: r["ended_at"])
    return "".join(f"{r['id']}\t{r['started_at']}\t{r['ended_at']}\t{r['status']}\t{r['stage']}\n" for r in rows)


def mirror(dest, volume, image, cfg, get_state=state, do_copy=copy_out, log=print):
    dest = os.path.realpath(os.path.expanduser(dest))
    s1 = get_state()
    if dest == LIVE:
        why = []
        if cfg.get("MIRROR_LIVE") != "1":
            why.append("MIRROR_LIVE=1 is not set in .env")
        if not os.path.exists(os.path.join(dest, "STOP")):
            why.append("fleetd's STOP flag is not in " + dest + " (fleetd would still be writing it)")
        if s1["stage"] == "shadow":
            why.append("the triage job is still in shadow")
        if why:
            log("MIRROR FAIL: refusing to write the live fleet folder: " + "; ".join(why))
            return 1
    if s1["running"]:
        log("MIRROR SKIP: a triage run is in flight")
        return 0
    last = newest_done(s1["runs"])
    if last is None:
        log("MIRROR SKIP: no finished triage run yet")
        return 0
    stamp_path = os.path.join(dest, "results", ".mirror_stamp")
    try:
        stamp = json.load(open(stamp_path))
    except (OSError, ValueError):
        stamp = {"end": ""}
    if last["ended_at"] <= stamp.get("end", ""):
        log(f"MIRROR SKIP: nothing new since {stamp['end']}")
        return 0
    start1 = newest_start(s1["runs"])
    with tempfile.TemporaryDirectory(dir=os.path.dirname(dest) or ".", prefix=".flotilla-mirror-") as tmp:
        do_copy(volume, image, tmp)
        s2 = get_state()
        if s2["running"] or newest_start(s2["runs"]) != start1:
            log("MIRROR RACED: a run started during the copy; discarded, next tick retries")
            return 0
        changed, removed = install(tmp, dest)
    rows = cycles_rows(s1["runs"], stamp.get("end", ""))
    cpath = os.path.join(dest, "results", "cycles.tsv")
    old = open(cpath, "rb").read() if os.path.exists(cpath) else b"run_id\tstart_utc\tend_utc\tstatus\tstage\n"
    if rows:
        write_atomic(cpath, old + rows.encode())
    write_atomic(stamp_path, json.dumps({"end": last["ended_at"], "run_id": last["id"],
                                         "copied_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}).encode() + b"\n")
    log(f"MIRROR PASS: {changed} files written, {removed} removed, {rows.count(chr(10))} cycle rows, stamp {last['ended_at']} ({last['id']})")
    return 0


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dest", default=None, help="default: MIRROR_DEST in .env, else ~/signal-fleet")
    a = ap.parse_args()
    cfg = env_file(os.path.join(H, ".env"))
    dest = a.dest or cfg.get("MIRROR_DEST") or LIVE
    volume = cfg.get("TRIAGE_STATE_VOLUME", "flotilla-triage-state-shadow")
    tags = [l.split("\t") for l in open(os.path.join(H, "deployed.tsv")) if l.strip()]
    tag = [t for t in tags if len(t) >= 4 and t[3].strip() == "pass"][-1][1]
    image = f"{cfg.get('FLOTILLA_REGISTRY', 'ghcr.io/chipi')}/flotilla-worker-triage:{tag}"
    try:
        sys.exit(mirror(dest, volume, image, cfg))
    except (subprocess.CalledProcessError, RuntimeError, OSError, ValueError) as e:
        err = getattr(e, "stderr", b"") or b""
        print(f"MIRROR FAIL: {e} {err.decode(errors='replace')[:300]}")
        sys.exit(1)


if __name__ == "__main__":
    main()
