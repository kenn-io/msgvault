package importer

import (
	"context"
	"errors"
	"fmt"
	"os"

	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/store"
)

// ImportICS archives a local calendar export under a stable caller-chosen source.
func ImportICS(ctx context.Context, st *store.Store, path, identifier string) (*store.ICalendarImportSummary, error) {
	raw, skipped, err := readICS(path)
	if err != nil {
		return nil, err
	}
	source, err := st.GetOrCreateSourceContext(ctx, "ics", identifier)
	if err != nil {
		return nil, fmt.Errorf("create ICS source: %w", err)
	}
	summary, err := st.PersistICalendarContext(ctx, source.ID, raw)
	if err != nil {
		return nil, err
	}
	summary.Skipped = skipped
	return &summary, nil
}

// readICS rejects a calendar with no readable event and returns the events it skips.
func readICS(path string) ([]byte, []error, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read ICS file: %w", err)
	}
	_, err = calevent.ParseICS(raw, "", "")
	var skipped *calevent.SkippedEvents
	if errors.As(err, &skipped) {
		return raw, skipped.Errs, nil
	}
	return raw, nil, err
}
