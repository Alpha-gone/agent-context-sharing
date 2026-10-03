//go:build !darwin && !linux

package authorize

import (
	"context"
)

func openBrowser(context.Context, string) error { return ErrAuthorization }
