package export

import (
	"path/filepath"
	"sync"
	"sync/atomic"
)

// looseBlobWrites counts loose blobs this process created, per resolved
// attachments directory. Scheduled ingest compares the count before and after
// a run to learn whether it left anything for the packer, without scanning
// the attachments table.
var looseBlobWrites sync.Map // resolved base dir -> *atomic.Int64

func looseBlobCounter(baseDir string) *atomic.Int64 {
	stored, _ := looseBlobWrites.LoadOrStore(baseDir, new(atomic.Int64))
	counter, _ := stored.(*atomic.Int64)
	return counter
}

func recordLooseBlobCreated(baseDir string, created bool) {
	if created {
		looseBlobCounter(baseDir).Add(1)
	}
}

// LooseBlobWrites reports how many loose attachment blobs this process has
// created under attachmentsDir. Deduplicated writes do not count.
func LooseBlobWrites(attachmentsDir string) int64 {
	baseDir, err := filepath.Abs(attachmentsDir)
	if err != nil {
		return 0
	}
	if resolved, err := filepath.EvalSymlinks(baseDir); err == nil {
		baseDir = resolved
	}
	return looseBlobCounter(baseDir).Load()
}
