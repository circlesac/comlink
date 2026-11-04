//go:build windows

package upstream

import (
	"context"
	"os"
	"os/exec"
	"syscall"
)

// shellCommand runs command with cmd /C. The command line is passed as is:
// Go's argument quoting is not what cmd.exe parses.
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	shell := os.Getenv("ComSpec")
	if shell == "" {
		shell = "cmd.exe"
	}
	cmd := exec.CommandContext(ctx, shell)
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd /C ` + command}
	return cmd
}
