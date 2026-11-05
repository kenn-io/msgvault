package deletion

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/msgvault/internal/fileutil"
)

type checkpoint struct {
	LastProcessedIndex int      `json:"last_processed_index"`
	Succeeded          int      `json:"succeeded"`
	Failed             int      `json:"failed"`
	FailedFrom         int      `json:"failed_from"`
	FailedIDs          []string `json:"failed_ids"`
	TombstoneIDs       []string `json:"tombstone_ids"`
}

func journalPath(manifestPath string) string {
	// Status moves must leave the journal reachable until its progress is folded.
	return filepath.Join(filepath.Dir(filepath.Dir(manifestPath)), strings.TrimSuffix(filepath.Base(manifestPath), ".json")+".checkpoint")
}

func appendCheckpoint(manifest *Manifest, path string, failedFrom int) error {
	e := manifest.Execution
	record := checkpoint{e.LastProcessedIndex, e.Succeeded, e.Failed, failedFrom, e.FailedIDs[failedFrom:], e.TombstoneIDs}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	path = journalPath(path)
	f, err := fileutil.SecureOpenFile(path, os.O_APPEND|os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	created := err == nil
	if errors.Is(err, os.ErrExist) {
		f, err = fileutil.SecureOpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	// Isolate a torn tail before appending the next complete record.
	if _, err := f.Write(append(append([]byte{'\n'}, data...), '\n')); err != nil {
		return fmt.Errorf("append checkpoint: %w", err)
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if created {
		return atomicfile.SyncDir(filepath.Dir(path))
	}
	return nil
}

func applyJournal(m *Manifest, data []byte) {
	if m.Execution == nil {
		return
	}
	for line := range bytes.SplitSeq(data, []byte{'\n'}) {
		var record checkpoint
		if err := json.Unmarshal(line, &record); err != nil {
			continue
		}
		e := m.Execution
		if record.FailedFrom < 0 || record.FailedFrom > len(e.FailedIDs) {
			continue
		}
		e.LastProcessedIndex = record.LastProcessedIndex
		e.Succeeded = record.Succeeded
		e.Failed = record.Failed
		e.FailedIDs = append(e.FailedIDs[:record.FailedFrom], record.FailedIDs...)
		e.TombstoneIDs = record.TombstoneIDs
	}
}
