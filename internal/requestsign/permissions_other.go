//go:build !unix && !windows

package requestsign

import (
	"errors"
	"os"
)

func privateFilePermissions(*os.File, os.FileInfo) error {
	return errors.New("private signing files are unsupported on this platform")
}
