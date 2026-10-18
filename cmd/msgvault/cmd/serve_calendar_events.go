package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/store"
)

func (a *storeAPIAdapter) ListCalendarOccurrencesContext(ctx context.Context, scope store.SourceScope, f store.CalendarOccurrenceFilter) (store.CalendarOccurrencePage, error) {
	return a.store.ListCalendarOccurrencesContext(ctx, scope, f)
}

func (a *storeAPIAdapter) GetCalendarOccurrenceContext(ctx context.Context, scope store.SourceScope, ref string) (store.CalendarOccurrence, error) {
	return a.store.GetCalendarOccurrenceContext(ctx, scope, ref)
}

var _ api.CalendarEventsBackend = (*storeAPIAdapter)(nil)
