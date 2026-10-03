package authorize

import (
	"context"
	"os"
	"os/exec"
)

func openBrowser(ctx context.Context, target string) error {
	wsl := os.Getenv("WSL_INTEROP") != "" || os.Getenv("WSL_DISTRO_NAME") != ""
	command, err := linuxBrowserCommand(ctx, target, wsl, exec.LookPath)
	if err != nil {
		return err
	}
	return runBrowserCommand(command)
}
