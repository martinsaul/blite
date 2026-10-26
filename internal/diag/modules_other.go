//go:build !windows

package diag

import (
	"context"
	"log/slog"
)

// WatchModules is Windows-only.
func WatchModules(context.Context, *slog.Logger) {}
