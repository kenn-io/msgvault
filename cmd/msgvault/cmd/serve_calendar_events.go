package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/store"
)

func (a *storeAPIAdapter) ListCalendarOccurrencesContext(ctx context.Context, query store.CalendarOccurrenceQuery) (store.CalendarOccurrencePage, error) {
	return a.store.ListCalendarOccurrencesContext(ctx, query)
}

var _ api.CalendarEventStore = (*storeAPIAdapter)(nil)

func (a *storeAPIAdapter) GetCalendarOccurrenceContext(ctx context.Context, ref string) (store.CalendarOccurrenceDetail, error) {
	return a.store.GetCalendarOccurrenceContext(ctx, ref)
}
