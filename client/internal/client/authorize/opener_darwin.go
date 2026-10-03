package authorize

import (
	"context"
	"io"
	"os/exec"
)

func openBrowser(ctx context.Context, target string) error {
	command := exec.CommandContext(ctx, "open", target)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	return command.Run()
}
