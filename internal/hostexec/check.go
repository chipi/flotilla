package hostexec

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/chipi/flotilla/internal/hostcfg"
)

// LocalChecker evaluates the precondition kinds a host agent knows (ADR-0006 §7: local only). An unknown
// kind is an error, so the execution is unknown and nothing runs.
type LocalChecker struct {
	Run Runner // ExecRunner on a host; a fake in tests
}

var launchdLabelRe = regexp.MustCompile(`^(system|gui/[0-9]+)/[A-Za-z0-9._-]+$`)
var launchdStateRe = regexp.MustCompile(`(?m)^\s*state = (.+)$`)

func (c LocalChecker) Check(ctx context.Context, p hostcfg.Pre) (bool, string, error) {
	switch p.Kind {
	case "launchd_not_running":
		// launchctl print reads a job's state without root (checked on the mini, macOS 15.7.7: the
		// operator's account read system/com.homelab.db-backup, "state = not running").
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
	default:
		return false, "", fmt.Errorf("precondition kind %q is not one this agent knows", p.Kind)
	}
}
