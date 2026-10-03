package authorize

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"
)

func runBrowserCommand(command *exec.Cmd) error {
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	// 실행기가 종료돼도 후손이 출력 파이프를 잡고 있는 경우 회수를 제한한다.
	command.WaitDelay = time.Second
	return command.Run()
}

func linuxBrowserCommand(ctx context.Context, target string, wsl bool, lookPath func(string) (string, error)) (*exec.Cmd, error) {
	program := "xdg-open"
	if wsl {
		program = "wslview"
	}
	path, err := lookPath(program)
	if err == nil {
		return exec.CommandContext(ctx, path, target), nil
	}
	if !wsl || !errors.Is(err, exec.ErrNotFound) {
		return nil, ErrAuthorization
	}
	path, err = lookPath("powershell.exe")
	if err != nil {
		return nil, ErrAuthorization
	}
	const script = `[Console]::InputEncoding = [System.Text.UTF8Encoding]::new($false); $target = [Console]::In.ReadToEnd(); Start-Process -FilePath $target -ErrorAction Stop`
	command := exec.CommandContext(ctx, path, "-NoProfile", "-NonInteractive", "-Command", script)
	command.Stdin = strings.NewReader(target)
	return command, nil
}
