//go:build !windows

package cmd

import (
	"context"
	"strconv"

	"go.kenn.io/kit/backup"
)

func escapeRestoreArg(arg string) string { return strconv.Quote(arg) }

func secureRestoredTokens(context.Context, *backup.Repo, string, string, bool) (int, error) {
	return 0, nil
}
