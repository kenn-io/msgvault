package daemonclient

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/contactcadence"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const DirectoryCadenceMinAPISchemaVersion = "3.43.0"

var _ peoplebrowser.DirectoryLister = (*PeopleBrowser)(nil)

// DirectoryAddressMinAPISchemaVersion adds live address filtering. Earlier
// daemons ignore the unknown parameter, so callers must check before using it.
const DirectoryAddressMinAPISchemaVersion = "3.29.0"

type directoryAddressUnsupportedError struct{}

func (directoryAddressUnsupportedError) Error() string {
	return fmt.Sprintf("person directory address filtering requires daemon API schema %s or newer; upgrade the daemon", DirectoryAddressMinAPISchemaVersion)
}

func (directoryAddressUnsupportedError) APIErrorCode() string {
	return "directory_address_unsupported"
}

// RequireDirectoryAddress checks support only when address filtering is used.
func (c *Client) RequireDirectoryAddress(ctx context.Context, address string) error {
	if strings.TrimSpace(address) == "" {
		return nil
	}
	supported, err := c.SupportsAPISchemaVersion(ctx, DirectoryAddressMinAPISchemaVersion)
	if err != nil {
		return fmt.Errorf("check person directory address capability: %w", err)
	}
	if !supported {
		return directoryAddressUnsupportedError{}
	}
	return nil
}

// ListDirectoryPeople forwards the Store Directory query to the daemon.
// The daemon owns filtering, ordering, limits, and cursor validation.
func (b *PeopleBrowser) ListDirectoryPeople(
	ctx context.Context,
	query store.DirectoryPeopleQuery,
) (*store.DirectoryPeoplePage, error) {
	if err := b.engine.store.RequireDirectoryAddress(ctx, query.Address); err != nil {
		return nil, err
	}
	if query.Cadence != "" || query.DueWithinDays != nil || query.Sort == store.DirectoryPeopleSortMostOverdue || query.Sort == store.DirectoryPeopleSortNextDue {
		supported, err := b.engine.store.SupportsAPISchemaVersion(ctx, DirectoryCadenceMinAPISchemaVersion)
		if err != nil {
			return nil, fmt.Errorf("check directory cadence capability: %w", err)
		}
		if !supported {
			return nil, fmt.Errorf("directory cadence requires daemon API schema %s or newer; upgrade the daemon", DirectoryCadenceMinAPISchemaVersion)
		}
	}
	options := &generated.ListDirectoryPeopleRequestOptions{
		Query: &generated.ListDirectoryPeopleQuery{
			Q:                 optionalString(query.Query),
			Address:           optionalString(strings.TrimSpace(query.Address)),
			Cadence:           optionalString(query.Cadence),
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
	if query.DueWithinDays != nil {
		days := int64(*query.DueWithinDays)
		options.Query.DueWithinDays = &days
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
	for _, preset := range resp.JSON200.CadencePresets {
		page.CadencePresets = append(page.CadencePresets, contactcadence.Preset{List: preset.List, Days: preset.Days})
	}
	if coverage := resp.JSON200.CadenceCoverage; coverage != nil {
		page.CadenceCoverage = &store.DirectoryCadenceCoverage{Total: int(coverage.Total), NoList: int(coverage.NoList), Unknown: int(coverage.Unknown), Scheduled: int(coverage.Scheduled)}
	}
	for i, person := range resp.JSON200.People {
		page.People[i] = store.DirectoryPersonSummary{
			ID:                 person.ID,
			VCardUID:           person.VcardUID,
			CardDAVBindings:    cardDAVBindingsFromGenerated(person.CarddavBindings),
			DisplayName:        person.DisplayName,
			Revision:           person.Revision,
			PrimaryChannel:     stringValue(person.PrimaryChannel),
			ContactState:       person.ContactState,
			LastContactAt:      copyTime(person.LastContactAt),
			LastContactChannel: stringValue(person.LastContactChannel),
			Categories:         append([]string{}, person.Categories...),
			Organizations:      append([]string{}, person.Organizations...),
		}
		if c := person.Cadence; c != nil {
			page.People[i].Cadence = store.DirectoryCadence{FrequencyDays: c.FrequencyDays, List: stringValue(c.List), DueAt: copyTime(c.DueAt), Status: c.Status, Stale: c.Stale, DaysOverdue: directoryInt(c.DaysOverdue)}
		}
		if c := person.ReachOutContext; c != nil {
			page.People[i].ReachOutContext = &store.DirectoryReachOutContext{AwaitingReplyCount: directoryInt(c.AwaitingReplyCount), NeedsReplyCount: directoryInt(c.NeedsReplyCount), OpenLoopsStale: c.OpenLoopsStale, SuggestedList: stringValue(c.SuggestedList), SignalsStale: c.SignalsStale}
		}
	}
	return page, nil
}

func directoryInt(value *int64) *int {
	if value == nil {
		return nil
	}
	n := int(*value)
	return &n
}
