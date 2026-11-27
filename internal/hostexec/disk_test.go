package hostexec

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chipi/flotilla/internal/hostcfg"
)

func fakeStatfs(avail, size uint64) func(string) (uint64, uint64, error) {
	return func(string) (uint64, uint64, error) { return avail, size, nil }
}

func TestDiskFreeRatioBelow(t *testing.T) {
	ctx := context.Background()
	pre := func(path, ratio string) hostcfg.Pre {
		return hostcfg.Pre{Kind: "disk_free_ratio_below", Args: map[string]string{"path": path, "ratio": ratio}}
	}
	for name, c := range map[string]struct {
		avail, size uint64
		ratio       string
		holds       bool
		detail      string
	}{
		"below":        {100, 1000, "0.15", true, "/data has 10.0% free, below 15.0%"},
		"above":        {300, 1000, "0.15", false, "/data has 30.0% free, not below 15.0%"},
		"exactly":      {150, 1000, "0.15", false, "/data has 15.0% free, not below 15.0%"},
		"a full disk":  {0, 1000, "0.05", true, "/data has 0.0% free, below 5.0%"},
		"ratio of one": {999, 1000, "1", true, "/data has 99.9% free, below 100.0%"},
	} {
		holds, detail, err := (LocalChecker{Statfs: fakeStatfs(c.avail, c.size)}).Check(ctx, pre("/data", c.ratio))
		if err != nil || holds != c.holds || detail != c.detail {
			t.Errorf("%s: %v %q %v", name, holds, detail, err)
		}
	}
	for name, p := range map[string]hostcfg.Pre{
		"a relative path":     pre("data", "0.15"),
		"a dotted path":       pre("/data/../etc", "0.15"),
		"a path with a space": pre("/data x", "0.15"),
		"a path with a quote": pre("/data'x", "0.15"),
		"ratio zero":          pre("/data", "0"),
		"ratio above one":     pre("/data", "1.5"),
		"ratio negative":      pre("/data", "-0.1"),
		"ratio not a number":  pre("/data", "ten"),
		"ratio NaN":           pre("/data", "NaN"),
		"ratio infinite":      pre("/data", "Inf"),
		"no path":             {Kind: "disk_free_ratio_below", Args: map[string]string{"ratio": "0.1"}},
	} {
		if _, _, err := (LocalChecker{Statfs: fakeStatfs(1, 10)}).Check(ctx, p); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// A read that fails, or a filesystem with no size, is no answer: the execution is unknown and nothing runs.
	failing := func(string) (uint64, uint64, error) { return 0, 0, os.ErrNotExist }
	if _, _, err := (LocalChecker{Statfs: failing}).Check(ctx, pre("/data", "0.15")); err == nil {
		t.Error("a failed statfs: no error")
	}
	if _, _, err := (LocalChecker{Statfs: fakeStatfs(0, 0)}).Check(ctx, pre("/data", "0.15")); err == nil {
		t.Error("a zero-size filesystem: no error")
	}
}

// The real read, on the test's own temp dir: the platform's statfs (linux or darwin) gives a ratio and no error.
func TestDiskFreeRatioReadsTheFilesystem(t *testing.T) {
	dir := t.TempDir()
	pre := hostcfg.Pre{Kind: "disk_free_ratio_below", Args: map[string]string{"path": dir, "ratio": "1"}}
	if _, detail, err := (LocalChecker{}).Check(context.Background(), pre); err != nil || !strings.HasPrefix(detail, dir+" has ") {
		t.Fatalf("%q %v", detail, err)
	}
	gone := hostcfg.Pre{Kind: "disk_free_ratio_below", Args: map[string]string{"path": filepath.Join(dir, "nope"), "ratio": "0.5"}}
	if _, _, err := (LocalChecker{}).Check(context.Background(), gone); err == nil {
		t.Error("a missing path: no error")
	}
}

func TestPathExists(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	pre := func(p string) hostcfg.Pre {
		return hostcfg.Pre{Kind: "path_exists", Args: map[string]string{"path": p}}
	}
	missing := filepath.Join(dir, "nope")
	for name, c := range map[string]struct {
		path   string
		holds  bool
		detail string
	}{
		"a directory":   {dir, true, dir + " exists"},
		"a file":        {file, true, file + " exists"},
		"a missing one": {missing, false, missing + " does not exist"},
	} {
		holds, detail, err := (LocalChecker{}).Check(ctx, pre(c.path))
		if err != nil || holds != c.holds || detail != c.detail {
			t.Errorf("%s: %v %q %v", name, holds, detail, err)
		}
	}
	for name, p := range map[string]string{"relative": "data", "dotted": "/a/../b", "space": "/a b", "empty": ""} {
		if _, _, err := (LocalChecker{}).Check(ctx, pre(p)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// A path that cannot be read (here, under a file) is no answer, not "does not exist".
	if _, _, err := (LocalChecker{}).Check(ctx, pre(filepath.Join(file, "x"))); err == nil {
		t.Error("a path under a file: no error")
	}
}
