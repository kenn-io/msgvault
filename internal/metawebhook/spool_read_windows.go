//go:build windows

package metawebhook

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// A delivery being moved may briefly reject a reader even when it still exists.
func transientSpoolReadError(err error) bool {
	if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return false
	}
	pathErr, ok := errors.AsType[*fs.PathError](err)
	if !ok {
		return false
	}
	name := filepath.Base(pathErr.Path)
	id := strings.TrimSuffix(strings.TrimSuffix(name, ".json"), ".state")
	return validID(id)
}
