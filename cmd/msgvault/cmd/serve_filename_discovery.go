package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/filenamediscovery"
	"go.kenn.io/msgvault/internal/store"
)

func (s *storeAPIAdapter) FilenamePersonContext(ctx context.Context, id int64) (*store.Person, error) {
	return s.store.FilenamePersonContext(ctx, id)
}
func (s *storeAPIAdapter) FilenameCandidatesContext(ctx context.Context, q filenamediscovery.Query, scope store.FilenameSuggestionScope) (store.FilenameCandidateSet, error) {
	return s.store.FilenameCandidatesContext(ctx, q, scope)
}
func (s *storeAPIAdapter) FilenameSuggestionRowsContext(ctx context.Context, ids []int64, scope store.FilenameSuggestionScope) ([]store.FilenameSuggestionRow, error) {
	return s.store.FilenameSuggestionRowsContext(ctx, ids, scope)
}
