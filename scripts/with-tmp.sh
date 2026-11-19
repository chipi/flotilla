#!/usr/bin/env bash
# Run a command with TMPDIR set to a fresh folder of its own under the user's cache, and remove the folder when the command ends, pass or
# fail (a trap), then check it is gone: /tmp is a shared tmpfs with a fixed number of inodes, and a test run that leaves its temp folders there
# fills it for every other user of the machine. Python's tempfile and Go's os.TempDir (a go run on this host) honour TMPDIR. A go test run by
# scripts/go.sh runs in its container, whose /tmp is the container's own and goes with it.
#   scripts/with-tmp.sh make test-unit
set -uo pipefail
# FLOTILLA_TEST_TMP_BASE, else the user's cache. On a host where mise shims run from under $HOME the cache is the wrong place: a test that
# starts a shimmed command in a folder below $HOME makes mise read $HOME's config as a project's and reach out to the network (the seam end-to-end's
# proxy saw api.github.com), so such a host sets FLOTILLA_TEST_TMP_BASE to a folder outside $HOME (/var/tmp/flotilla-tests-$USER is on disk).
base="${FLOTILLA_TEST_TMP_BASE:-${XDG_CACHE_HOME:-$HOME/.cache}/flotilla-tests}"
mkdir -p "$base"
run="$base/$(date +%Y%m%d-%H%M%S)-$$"
mkdir "$run"
cleanup() { rm -rf "$run"; }
trap cleanup EXIT
trap 'exit 130' INT TERM
TMPDIR="$run" "$@"
rc=$?
cleanup
if [ -e "$run" ]; then
  echo "with-tmp: $run was not removed" >&2
  exit 1
fi
echo "with-tmp: $run removed"
exit $rc
