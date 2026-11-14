//go:build !windows

package cmd

import (
	"context"

	"go.kenn.io/kit/backup"
)

func secureRestoredTokens(context.Context, *backup.Repo, string, string, bool) (int, error) {
	return 0, nil
}
