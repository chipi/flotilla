#!/usr/bin/env python3
"""The e2e's generated shadow versions: a v2 beside the v1 of every example routine the stack can shadow.

  make_shadows.py SPECS_DIR [WORKER ...]     (default WORKER: ops-read)

The example fleets ship v1 only (a version that does something different is the installation's to write). The no-effect matrix
needs a shadow to run, so for each routine folder <fleet>/<routine>/ under SPECS_DIR that names one of the WORKERs, has a v1 and no v2, and is
not in the e2e's own fleet, v2 is a copy of v1 plus a marker file: the version's hash is over its files, so the copy would otherwise be v1 itself
(a version that is its own live version is no shadow). The v2 runs the example's real command, so what the matrix proves is that the real code,
run as a shadow, changes nothing outside its record.

Prints one line per generated routine (fleet/routine), then "GENERATED n".
"""
import os
import shutil
import sys

MARKER = "shadow-marker.txt"
SKIP_FLEETS = {"e2e"}


def generate(specs, workers, dry=False):
    made = []
    for fleet in sorted(os.listdir(specs)):
        fdir = os.path.join(specs, fleet)
        if fleet in SKIP_FLEETS or not os.path.isdir(fdir):
            continue
        for routine in sorted(os.listdir(fdir)):
            rdir = os.path.join(fdir, routine)
            yml = os.path.join(rdir, "routine.yaml")
            if not os.path.isfile(yml):
                continue
            worker = ""
            with open(yml) as f:
                for line in f:
                    if line.startswith("worker:"):
                        worker = line.split(":", 1)[1].strip()
            v1, v2 = os.path.join(rdir, "v1"), os.path.join(rdir, "v2")
            if worker not in workers or not os.path.isdir(v1) or os.path.exists(v2):
                continue
            if dry:
                made.append(fleet + "/" + routine)
                continue
            shutil.copytree(v1, v2)
            with open(os.path.join(v2, MARKER), "w") as f:
                f.write("The e2e's generated shadow: v1's files, and this one so that the version's hash differs.\n")
            made.append(fleet + "/" + routine)
    return made


def main(argv):
    if len(argv) < 2:
        print(__doc__)
        return 2
    made = generate(argv[1], set(argv[2:]) or {"ops-read"})
    for m in made:
        print(m)
    print("GENERATED %d" % len(made))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
