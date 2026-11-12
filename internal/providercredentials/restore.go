package providercredentials

import (
	"errors"
	"fmt"
	"path/filepath"
)

// ErrRestorePermissionsConflict means an omitted token would lose inherited access.
var ErrRestorePermissionsConflict = errors.New("restored token permissions conflict with an omitted entry")

// SecureRestored protects only direct token files confirmed in the restored snapshot.
func SecureRestored(tokenDir string, restored []string) error {
	if len(restored) == 0 {
		return nil
	}
	selected := make(map[string]struct{}, len(restored))
	for _, name := range restored {
		selected[name] = struct{}{}
	}
	if err := restorePreflight(tokenDir, selected); err != nil {
		return err
	}
	for _, name := range restored {
		if err := secureRestoredFile(filepath.Join(tokenDir, name)); err != nil {
			return fmt.Errorf("secure restored token %q: %w", name, err)
		}
	}
	return withStoreLock(tokenDir, func() error { return (nativePermissions{}).secureDirectory(tokenDir) })
}
