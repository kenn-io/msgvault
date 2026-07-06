package backup

import (
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/kit/pack"
)

// writeFileAtomic publishes data at finalRel (relative to the repo root)
// via staging -> fsync -> rename -> parent dir sync, so a crash never
// leaves a partially written repo object at its final path.
func writeFileAtomic(r *Repo, finalRel string, data []byte) error {
	final := r.Path(finalRel)
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return fmt.Errorf("backup: creating parent dir for %s: %w", finalRel,
			err)
	}
	tmp, err := os.CreateTemp(r.Path(stagingDirName), filepath.Base(finalRel)+
		".*")
	if err != nil {
		return fmt.Errorf("backup: creating staging file for %s: %w", finalRel,
			err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("backup: writing staging file for %s: %w", finalRel,
			err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("backup: syncing staging file for %s: %w", finalRel,
			err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("backup: closing staging file for %s: %w", finalRel,
			err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		cleanup()
		return fmt.Errorf("backup: publishing %s: %w", finalRel, err)
	}
	if err := pack.SyncDir(filepath.Dir(final)); err != nil {
		return fmt.Errorf(
			"backup: object published at %s but directory sync failed "+
				"(entry may not be durable): %w", final, err)
	}
	return nil
}
