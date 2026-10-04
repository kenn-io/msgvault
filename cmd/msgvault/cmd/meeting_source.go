package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"go.kenn.io/msgvault/internal/meetingidentity"
	"go.kenn.io/msgvault/internal/store"
)

// registerMeetingSource registers a stable source label and confirms its
// configured primary email even when the source already has aliases. Meeting
// providers require a primary identity, so unlike confirmDefaultIdentity this
// path is neither best-effort nor suppressed by existing identity rows.
func registerMeetingSource(
	out io.Writer,
	s *store.Store,
	sourceType string,
	identifier string,
	accountEmail string,
) (*store.Source, error) {
	primary := meetingidentity.Normalize(accountEmail)
	if primary == "" {
		return nil, errors.New("meeting account email is required")
	}
	source, err := s.GetOrCreateSource(sourceType, identifier)
	if err != nil {
		return nil, fmt.Errorf("create source: %w", err)
	}
	if err := s.UpdateSourceDisplayName(source.ID, identifier); err != nil {
		return nil, fmt.Errorf("set display name: %w", err)
	}
	if err := s.AddAccountIdentity(source.ID, primary, "account-email"); err != nil {
		return nil, fmt.Errorf("confirm meeting account identity: %w", err)
	}
	_, _ = fmt.Fprintf(out, "Confirmed identity %s on %s (signal: account-email).\n", primary, identifier)
	commandSource := strings.ReplaceAll(sourceType, "_", "-")
	_, _ = fmt.Fprintf(out, "After identity changes, run msgvault sync-%s %s --full to refresh existing meeting attribution.\n",
		commandSource, identifier)
	return source, nil
}

// meetingSources resolves CLI identifiers against one provider's configured
// entries. lookup is the config getter, which matches case-insensitively; a nil
// lookup means the configuration was unavailable.
type meetingSources[T any] struct {
	table      string // config table name, such as "notion_meetings"
	hint       string
	configured []T
	lookup     func(string) *T
	identifier func(T) string
}

func (m meetingSources[T]) missing() error {
	return errors.New("no [[" + m.table + "]] sources configured\n\n" + m.hint)
}

// one picks the entry an optional argument names; with no argument there
// must be exactly one entry.
func (m meetingSources[T]) one(args []string) (*T, error) {
	if m.lookup == nil {
		return nil, errors.New("configuration is unavailable")
	}
	if len(m.configured) == 0 {
		return nil, m.missing()
	}
	if len(args) > 0 {
		src := m.lookup(args[0])
		if src == nil {
			ids := make([]string, 0, len(m.configured))
			for _, candidate := range m.configured {
				ids = append(ids, m.identifier(candidate))
			}
			return nil, fmt.Errorf("no [[%s]] entry with identifier %q (configured: %s)", m.table, args[0], strings.Join(ids, ", "))
		}
		return src, nil
	}
	if len(m.configured) > 1 {
		return nil, fmt.Errorf("multiple [[%s]] sources configured; pass an identifier", m.table)
	}
	src := m.configured[0]
	return &src, nil
}

// selected picks the named entry, the only entry, or every entry.
func (m meetingSources[T]) selected(args []string) ([]T, error) {
	if m.lookup != nil && len(args) == 0 && len(m.configured) != 1 {
		if len(m.configured) == 0 {
			return nil, m.missing()
		}
		return m.configured, nil
	}
	src, err := m.one(args)
	if err != nil {
		return nil, err
	}
	return []T{*src}, nil
}

// finishMeetingImport reports a failed or canceled meeting sync, first
// refreshing the cache when the run committed writes.
func finishMeetingImport(provider, identifier string, writes int64, importErr, cancelErr error, refresh func() error) error {
	var operationErr error
	switch {
	case cancelErr != nil:
		operationErr = fmt.Errorf("%s sync %s canceled: %w", provider, identifier, cancelErr)
	case importErr != nil:
		operationErr = fmt.Errorf("%s sync %s failed: %w", provider, identifier, importErr)
	default:
		return nil
	}
	var refreshErr error
	if writes > 0 && refresh != nil {
		refreshErr = refresh()
	}
	return errors.Join(operationErr, refreshErr)
}

// finishScheduledMeetingImport reports a scheduled run and refreshes the
// cache under a context detached from the job, after a failure too.
func finishScheduledMeetingImport(
	ctx context.Context,
	provider, identifier, cacheKey string,
	writes int64,
	importErr, cancelErr error,
	refreshCache func(context.Context, string) error,
) error {
	refreshCtx := context.WithoutCancel(ctx)
	refresh := func() error {
		if refreshCache == nil {
			return nil
		}
		return refreshCache(refreshCtx, cacheKey)
	}
	if err := finishMeetingImport(provider, identifier, writes, importErr, cancelErr, refresh); err != nil {
		return err
	}
	return refresh()
}

// requireRegisteredMeetingSource stops a scheduled sync before the importer's
// GetOrCreateSource would create a source the user never added.
func requireRegisteredMeetingSource(st *store.Store, sourceType, identifier string, missing error) error {
	_, err := st.GetSourceByTypeAndIdentifier(sourceType, identifier)
	if errors.Is(err, store.ErrSourceNotFound) {
		return missing
	}
	return err
}
