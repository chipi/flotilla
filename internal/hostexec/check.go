package hostexec

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/chipi/flotilla/internal/hostcfg"
)

// LocalChecker evaluates the precondition kinds a host agent knows (ADR-0006 §7: local only). An unknown
// kind is an error, so the execution is unknown and nothing runs.
type LocalChecker struct {
	Run    Runner                                            // ExecRunner on a host; a fake in tests
	Statfs func(path string) (avail, size uint64, err error) // the platform's statfs when nil; a fake in tests
}

var launchdLabelRe = regexp.MustCompile(`^(system|gui/[0-9]+)/[A-Za-z0-9._-]+$`)
var launchdStateRe = regexp.MustCompile(`(?m)^\s*state = (.+)$`)

func (c LocalChecker) Check(ctx context.Context, p hostcfg.Pre) (bool, string, error) {
	switch p.Kind {
	case "launchd_not_running":
		// launchctl print reads a job's state without root (checked on the host, macOS 15.7.7: the
		// operator's account read system/com.example.db-backup, "state = not running").
		label := p.Args["label"]
		if !launchdLabelRe.MatchString(label) {
			return false, "", fmt.Errorf("launchd label %q", label)
		}
		exit, out, timedOut, err := c.Run.Run(ctx, []string{"/bin/launchctl", "print", label}, 10*time.Second)
		if err != nil || timedOut || exit != 0 {
			return false, "", fmt.Errorf("launchctl print %s: exit %d, %v", label, exit, err)
		}
		m := launchdStateRe.FindSubmatch(out)
		if m == nil {
			return false, "", fmt.Errorf("launchctl print %s: no state line", label)
		}
		state := strings.TrimSpace(string(m[1]))
		return state != "running", label + " is " + state, nil
	case "disk_free_ratio_below":
		// Holds while the filesystem at path has less than ratio of its space available. Read with statfs, no command
		// runs; a path or a read that fails is an error (unknown), never "holds" or "does not hold".
		path, ratio, err := hostcfg.DiskRatioArgs(p.Args)
		if err != nil {
			return false, "", err
		}
		read := c.Statfs
		if read == nil {
			read = statfs
		}
		avail, size, err := read(path)
		if err != nil {
			return false, "", fmt.Errorf("statfs %s: %v", path, err)
		}
		if size == 0 {
			return false, "", fmt.Errorf("statfs %s: a filesystem with no size", path)
		}
		free := float64(avail) / float64(size)
		if free < ratio {
			return true, fmt.Sprintf("%s has %.1f%% free, below %.1f%%", path, free*100, ratio*100), nil
		}
		return false, fmt.Sprintf("%s has %.1f%% free, not below %.1f%%", path, free*100, ratio*100), nil
	case "path_exists":
		path, err := hostcfg.PathArg(p.Args["path"])
		if err != nil {
			return false, "", err
		}
		switch _, err := os.Stat(path); {
		case err == nil:
			return true, path + " exists", nil
		case errors.Is(err, fs.ErrNotExist):
			return false, path + " does not exist", nil
		default: // permission, a component that is a file: the answer is not known
			return false, "", fmt.Errorf("stat %s: %v", path, err)
		}
	default:
		return false, "", fmt.Errorf("precondition kind %q is not one this agent knows", p.Kind)
	}
}
