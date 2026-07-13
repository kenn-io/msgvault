package cmd

import (
	"errors"
	"fmt"
	"io"
	"strings"

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
	primary := strings.ToLower(strings.TrimSpace(accountEmail))
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
	_, _ = fmt.Fprintf(out, "After identity changes, run msgvault sync-%s %s --full to refresh existing meeting attribution.\n",
		sourceType, identifier)
	return source, nil
}
