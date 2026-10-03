package authorize

import (
	"context"
	"os/exec"
)

func openBrowser(ctx context.Context, target string) error {
	return runBrowserCommand(exec.CommandContext(ctx, "open", target))
}
