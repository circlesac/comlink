//go:build unix

package upstream

import (
	"context"
	"os/exec"
	"syscall"
)

// shellCommand runs command with sh -c in a process group of its own, which
// a timeout kills whole: the shell and anything it started.
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return cmd
}
