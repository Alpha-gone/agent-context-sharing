package authorize

import (
	"context"
	"io"
	"os"
	"os/exec"
)

func openBrowser(ctx context.Context, target string) error {
	program := "xdg-open"
	if os.Getenv("WSL_INTEROP") != "" || os.Getenv("WSL_DISTRO_NAME") != "" {
		program = "wslview"
	}
	command := exec.CommandContext(ctx, program, target)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	return command.Run()
}
