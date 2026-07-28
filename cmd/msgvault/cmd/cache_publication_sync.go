package cmd

import (
	"errors"
	"os"
	"path/filepath"
)

func writeDurableFile(path string, data []byte, mode os.FileMode) error {
	// #nosec G703 -- callers construct path from fixed filenames below private transaction/cache roots.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Close(); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}
