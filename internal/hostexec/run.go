package hostexec

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// ExecRunner runs an argv directly (exec, never a shell) in its own process group, so a timeout kills
// the command and every child it started.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, argv []string, timeout time.Duration) (int, []byte, bool, error) {
	if len(argv) == 0 {
		return -1, nil, false, errors.New("empty argv")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LANG=C"} // built, never inherited
	if err := cmd.Start(); err != nil {
		return -1, nil, false, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return exitCode(err), out.Bytes(), false, nil
	case <-timer.C:
	case <-ctx.Done():
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
	return -1, out.Bytes(), true, nil
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}
