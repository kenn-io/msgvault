package carddav

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"go.kenn.io/msgvault/internal/icloudurl"
	"go.kenn.io/msgvault/internal/store"
)

// ICloudDiscoveryURL is Apple's fixed CardDAV bootstrap endpoint.
const ICloudDiscoveryURL = "https://contacts.icloud.com/"

// ErrICloudReconnectRequired leaves non-default book choices and owned remote
// state for an explicit reconnect rather than replacing them during sync.
var ErrICloudReconnectRequired = errors.New("iCloud moved your contacts; review address-book choices, published contacts and conflicts, then reconnect with add-carddav --icloud")

// IsICloudDiscoveryURL recognizes only the published HTTPS bootstrap root.
func IsICloudDiscoveryURL(raw string) bool {
	return icloudurl.IsDiscoveryURL(raw)
}

// ICloudPartitionOrigin returns the canonical HTTPS origin of an Apple
// contacts partition. Paths are allowed, but query, userinfo and fragments
// cannot be part of an iCloud discovery or credential binding.
func ICloudPartitionOrigin(raw string) (string, bool) {
	return icloudurl.PartitionOrigin(raw)
}

// ICloudPartitionRootOrigin returns the canonical origin only for a partition
// root URL, with an optional slash.
func ICloudPartitionRootOrigin(raw string) (string, bool) {
	return icloudurl.PartitionRootOrigin(raw)
}

// NewICloudService uses a restricted bootstrap for the published root and
// ordinary same-origin discovery for a previously selected partition. Raw
// upstream bodies are omitted from logs because they can identify an account.
func NewICloudService(st *store.Store, client *Client) *Service {
	if client == nil {
		return NewService(st, nil)
	}
	clone := *client
	clone.strictDiscovery = true
	clone.redactResponseBody = true
	clone.respectRetryAfter = true
	return NewRemoteService(st, &davRemote{client: &clone, icloud: true})
}

func discoverICloud(ctx context.Context, client *Client, entered string) (Discovery, error) {
	if !IsICloudDiscoveryURL(entered) || !IsICloudDiscoveryURL(client.origin.String()) || client.bearerToken != nil {
		return Discovery{}, ErrUnsafeTarget
	}
	ctx, cancel := context.WithTimeout(ctx, client.operationTimeout)
	defer cancel()
	budget := &Budget{remaining: client.operationBytes}
	bootstrap, _ := url.Parse(ICloudDiscoveryURL)
	client = iCloudClientAtOrigin(client, bootstrap)
	principal, err := discoverHref(ctx, client, bootstrap, CurrentUserPrincipalProperty, budget)
	if err != nil {
		return Discovery{}, err
	}
	if principal == nil {
		return Discovery{}, errors.New("iCloud discovery did not return current-user-principal")
	}
	homes, err := discoverHrefsUsingResolver(ctx, client, principal, AddressbookHomeSetProperty, budget,
		func(_ context.Context, _ *Client, base *url.URL, href string) (*url.URL, error) {
			if href == "" || strings.TrimSpace(href) != href || strings.Contains(href, "\\") {
				return nil, ErrUnsafeHref
			}
			home, parseErr := base.Parse(href)
			if parseErr != nil {
				return nil, ErrUnsafeHref
			}
			if _, ok := ICloudPartitionOrigin(home.String()); !ok {
				return nil, discoveryOriginError(home)
			}
			return home, nil
		})
	if err != nil {
		return Discovery{}, err
	}
	if len(homes) != 1 {
		return Discovery{}, errors.New("iCloud discovery requires one address-book home")
	}
	partition := iCloudClientAtOrigin(client, homes[0])
	books, err := discoverBooks(ctx, partition, homes[0], budget)
	if err != nil {
		return Discovery{}, err
	}
	return Discovery{PrincipalURL: principal, HomeURL: homes[0], HomeURLs: homes, Books: books}, nil
}

func iCloudClientAtOrigin(client *Client, target *url.URL) *Client {
	clone := *client
	clone.origin = originURL(target)
	clone.digest = new(digestState)
	// Bootstrap trust never extends an operator's private-destination exception
	// to a second origin. Every Apple destination still needs public DNS/IPs.
	clone.trustedOrigin, clone.trustedAddresses = nil, nil
	return &clone
}

type iCloudDiscoveryError struct{ cause error }

func (e *iCloudDiscoveryError) Error() string {
	if diagnostic, ok := errors.AsType[*DiscoveryOriginError](e.cause); ok {
		return diagnostic.Error()
	}
	if status, ok := errors.AsType[*StatusError](e.cause); ok {
		return fmt.Sprintf("iCloud discovery failed (HTTP %d)", status.StatusCode)
	}
	return "iCloud discovery failed"
}

func (e *iCloudDiscoveryError) Unwrap() error { return e.cause }
