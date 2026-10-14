package importer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/store"
)

// ImportICS archives a local calendar export under a stable caller-chosen source.
func ImportICS(ctx context.Context, st *store.Store, path, identifier string) (*store.ICalendarImportSummary, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return nil, errors.New("--identifier is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open ICS file: %w", err)
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, calevent.MaxICSBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read ICS file: %w", err)
	}
	if len(raw) > calevent.MaxICSBytes {
		return nil, errors.New("ICS file exceeds 4 MiB limit")
	}
	if _, err := calevent.ParseICS(raw, "", calevent.SourceContext{}); err != nil {
		return nil, err
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
