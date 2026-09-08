package daemonclient

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

var _ peoplebrowser.DirectoryLister = (*PeopleBrowser)(nil)

// ListDirectoryPeople forwards the Store Directory query to the daemon.
// The daemon owns filtering, ordering, limits, and cursor validation.
func (b *PeopleBrowser) ListDirectoryPeople(
	ctx context.Context,
	query store.DirectoryPeopleQuery,
) (*store.DirectoryPeoplePage, error) {
	options := &generated.ListDirectoryPeopleRequestOptions{
		Query: &generated.ListDirectoryPeopleQuery{
			Q:                 optionalString(query.Query),
			Cursor:            optionalString(query.Cursor),
			Limit:             int64FromInt(query.Limit),
			ContactState:      optionalString(query.ContactState),
			Category:          optionalString(query.Category),
			Organization:      optionalString(query.Organization),
			PrimaryChannel:    optionalString(query.PrimaryChannel),
			LastContactAfter:  query.LastContactAfter,
			LastContactBefore: query.LastContactBefore,
		},
	}
	if query.Sort != "" {
		sort := generated.ListDirectoryPeopleQuerySort(query.Sort)
		options.Query.Sort = &sort
	}

	resp, err := APIResponse(b.engine.store,
		func(client *apiclient.Client) (*generated.ListDirectoryPeopleResp, error) {
			return client.ListDirectoryPeopleWithResponse(ctx, options)
		})
	if err != nil {
		return nil, err
	}
	if resp.JSON200 == nil {
		return nil, errors.New("empty directory people response")
	}

	page := &store.DirectoryPeoplePage{
		People:     make([]store.DirectoryPersonSummary, len(resp.JSON200.People)),
		NextCursor: stringValue(resp.JSON200.NextCursor),
	}
	for i, person := range resp.JSON200.People {
		page.People[i] = store.DirectoryPersonSummary{
			ID:             person.ID,
			DisplayName:    person.DisplayName,
			Revision:       person.Revision,
			PrimaryChannel: stringValue(person.PrimaryChannel),
			ContactState:   person.ContactState,
			LastContactAt:  copyTime(person.LastContactAt),
			Categories:     append([]string{}, person.Categories...),
			Organizations:  append([]string{}, person.Organizations...),
		}
	}
	return page, nil
}
