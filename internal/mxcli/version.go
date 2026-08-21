// internal/mxcli/version.go
package mxcli

import (
	"context"
	"time"
)

func Version(ctx context.Context) (string, error) {
	return run(ctx, 10*time.Second, "--version")
}