package hostcfg

import (
	"os"
	"strings"
	"testing"
)

const launchdPre = `{"kind": "launchd_not_running", "args": {"label": "system/com.example.db-backup"}}`

// The disk preconditions (RFC-0016 step 1): read-only kinds the agent checks with statfs and stat.
func TestDiskPreconditions(t *testing.T) {
	good, _ := os.ReadFile("testdata/host-01.json")
	with := func(pre string) []byte {
		body := strings.Replace(string(good), launchdPre, pre, 1)
		if body == string(good) {
			t.Fatal("the test's edit did not apply")
		}
		return []byte(body)
	}
	for name, pre := range map[string]string{
		"free ratio":  `{"kind": "disk_free_ratio_below", "args": {"path": "/var/lib/docker", "ratio": "0.15"}}`,
		"root":        `{"kind": "disk_free_ratio_below", "args": {"path": "/", "ratio": "1"}}`,
		"path exists": `{"kind": "path_exists", "args": {"path": "/opt/data"}}`,
		"both":        launchdPre + `, {"kind": "path_exists", "args": {"path": "/opt/data"}}`,
	} {
		if _, err := Parse(with(pre)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, pre := range map[string]string{
		"a relative path":      `{"kind": "disk_free_ratio_below", "args": {"path": "data", "ratio": "0.15"}}`,
		"a dotted path":        `{"kind": "disk_free_ratio_below", "args": {"path": "/a/../b", "ratio": "0.15"}}`,
		"a path with a quote":  `{"kind": "path_exists", "args": {"path": "/a'b"}}`,
		"ratio zero":           `{"kind": "disk_free_ratio_below", "args": {"path": "/", "ratio": "0"}}`,
		"ratio above one":      `{"kind": "disk_free_ratio_below", "args": {"path": "/", "ratio": "1.01"}}`,
		"ratio text":           `{"kind": "disk_free_ratio_below", "args": {"path": "/", "ratio": "low"}}`,
		"no ratio":             `{"kind": "disk_free_ratio_below", "args": {"path": "/"}}`,
		"no path":              `{"kind": "path_exists", "args": {}}`,
		"an extra arg":         `{"kind": "path_exists", "args": {"path": "/", "x": "y"}}`,
		"free ratio on a path": `{"kind": "disk_free_ratio_below", "args": {"path": "/", "ratio": "0.1", "label": "x"}}`,
	} {
		if _, err := Parse(with(pre)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
