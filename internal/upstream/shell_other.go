//go:build !unix && !windows

package upstream

import (
	"context"
	"os/exec"
)

// shellCommand runs command with sh -c.
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "/bin/sh", "-c", command)
}
