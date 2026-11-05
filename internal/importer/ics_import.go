package importer

import (
	"context"
	"fmt"
	"os"

	"go.kenn.io/msgvault/internal/store"
)

// ImportICS archives a local calendar export under a stable caller-chosen source.
func ImportICS(ctx context.Context, st *store.Store, path, identifier string) (*store.ICalendarImportSummary, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read ICS file: %w", err)
	}
	source, err := st.GetOrCreateSource("ics", identifier)
	if err != nil {
		return nil, fmt.Errorf("create ICS source: %w", err)
	}
	summary, err := st.PersistICalendarContext(ctx, source.ID, raw)
	if err != nil {
		return nil, err
	}
	return &summary, nil
}
