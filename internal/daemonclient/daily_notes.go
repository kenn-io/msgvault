package daemonclient

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/timeline"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func (c *Client) GetDailyNoteContext(ctx context.Context, date string, id int64) (*store.DailyNoteEntry, error) {
	return timeline.ReadDailyNote(ctx, c, date, id)
}

func (c *Client) ListDailyNoteEntriesContext(ctx context.Context, date string, limit, offset int) ([]store.DailyNoteEntry, error) {
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.ListDayEntriesResp, error) {
		return client.ListDayEntriesWithResponse(ctx, &generated.ListDayEntriesRequestOptions{PathParams: &generated.ListDayEntriesPath{Date: date}, Query: &generated.ListDayEntriesQuery{Limit: new(int64(limit)), Offset: new(int64(offset))}})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("daily notes: empty response")
	}
	entries := make([]store.DailyNoteEntry, 0, len(response.JSON200.Entries))
	for _, e := range response.JSON200.Entries {
		entries = append(entries, store.DailyNoteEntry{ID: e.ID, LocalDate: e.LocalDate, Ordinal: e.Ordinal, Body: e.Body, Author: e.Author, Source: e.Source, SourceRef: e.SourceRef, PersonIDs: e.PersonIds, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt})
	}
	return entries, nil
}
