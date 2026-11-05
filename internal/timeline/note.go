package timeline

import (
	"context"
	"fmt"

	"go.kenn.io/msgvault/internal/store"
)

// ReadDailyNote opens one authored entry through the existing day reader.
func ReadDailyNote(ctx context.Context, source NotesSource, date string, id int64) (*store.DailyNoteEntry, error) {
	if !store.IsValidLocalDate(date) || id <= 0 {
		return nil, ErrInvalidRequest
	}
	for offset := 0; ; offset += 100 {
		entries, err := source.ListDailyNoteEntriesContext(ctx, date, 100, offset)
		if err != nil {
			return nil, fmt.Errorf("read daily note: %w", err)
		}
		for _, entry := range entries {
			if entry.ID == id {
				return &entry, nil
			}
		}
		if len(entries) < 100 {
			return nil, store.ErrDailyNoteEntryNotFound
		}
	}
}
