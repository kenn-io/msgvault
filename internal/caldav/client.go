// Package caldav archives calendar resources through the shared DAV reader.
package caldav

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/carddav"
)

type Client struct {
	dav *carddav.Client
}

func NewClient(options carddav.ClientOptions) (*Client, error) {
	dav, err := carddav.NewClient(options)
	if err != nil {
		return nil, err
	}
	return &Client{dav: dav}, nil
}

func (c *Client) discover(ctx context.Context, entered string, budget *carddav.Budget) ([]carddav.DiscoveredBook, error) {
	discovery, err := carddav.DiscoverCollections(ctx, c.dav, carddav.CalendarKind, entered, budget)
	return discovery.Books, err
}

// pull reads a calendar's resources, restarting from a full read when the
// server rejects the saved sync token.
func (c *Client) pull(ctx context.Context, cal carddav.DiscoveredBook, token string, budget *carddav.Budget) (carddav.MemberPlan, error) {
	collection := carddav.Collection{URL: cal.URL.String(), SupportsSync: cal.SupportsSyncCollection, SupportsMultiget: cal.SupportsMultiget}
	plan, err := c.dav.PullMembers(ctx, carddav.CalendarKind, collection, token, budget)
	if errors.Is(err, carddav.ErrInvalidSyncToken) {
		return c.dav.PullMembers(ctx, carddav.CalendarKind, collection, "", budget)
	}
	return plan, err
}
