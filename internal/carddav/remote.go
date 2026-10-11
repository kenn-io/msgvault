package carddav

import (
	"context"
	"errors"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// ErrInvalidSyncToken reports that the server no longer accepts a saved sync
// token. The Service then pulls the book again with an empty token.
var ErrInvalidSyncToken = errors.New("CardDAV sync token is no longer valid")

// Remote is the network side of one contacts connection. The Service keeps
// the run history, retry gate, publication ledger and conflict capture, and
// calls a Remote only to read and write address books.
//
// Errors keep their CardDAV meaning, because the Service branches on them: a
// *StatusError with 404 or 410 for an absent card, 412 for a failed version
// check, and 429 or RetryAfter for a pause. A Remote that sends more than one
// request in a call sends each one through GateRequest.
type Remote interface {
	// Discover lists the address books that the entered URL reaches.
	Discover(ctx context.Context, entered string) (Discovery, error)
	// Pull returns the changes in book since token. An empty token returns a
	// complete snapshot.
	Pull(ctx context.Context, book store.CardDAVAddressBook, token string, budget *Budget) (store.CardDAVSyncPlan, error)
	// Get returns the current card at href, or absent when there is none.
	Get(ctx context.Context, href string) (resource store.CardDAVRemoteResource, absent bool, err error)
	// Put writes body to href. With create, it fails with 412 when href
	// exists. Otherwise it fails with 412 when etag is not current. A failure
	// before the write request is sent includes ErrWriteNotSent. A card
	// larger than a Microsoft contact can hold fails with 413 and
	// ErrMicrosoftContactTooLarge.
	Put(ctx context.Context, href string, body []byte, etag string, create bool) error
	// Delete removes href, and fails with 412 when etag is not current. A
	// failure before the request is sent includes ErrWriteNotSent.
	Delete(ctx context.Context, href, etag string) error
	// CreateHref returns the href that a new card with uid gets in the book
	// at collectionURL.
	CreateHref(collectionURL, uid string) (string, error)
	// Limits returns the time limit and byte budget of one operation.
	Limits() (time.Duration, int64)
}

// NewRemoteService runs the shared sync, publication and conflict logic
// against remote.
func NewRemoteService(st *store.Store, remote Remote) *Service {
	return &Service{store: st, remote: remote}
}

type gateKey struct{}

// GateRequest runs one request of a Remote call behind the connection's retry
// gate. A pause from one request then stops the next request of the same
// call, also when the Remote would fall back to another request shape.
func GateRequest(ctx context.Context, request func(context.Context) error) error {
	if s, ok := ctx.Value(gateKey{}).(*Service); ok {
		return s.gate(ctx, request)
	}
	return request(ctx)
}

// gate runs one remote call behind the connection's retry gate, and saves a
// new gate when the remote asks for a pause.
func (s *Service) gate(ctx context.Context, call func(context.Context) error) error {
	if err := s.checkRetry(ctx); err != nil {
		return err
	}
	err := call(context.WithValue(ctx, gateKey{}, s))
	if status := retryStatus(err); status != nil {
		if gateErr := s.setRetry(ctx, time.Now().Add(status.RetryAfter).UTC()); gateErr != nil {
			return errors.Join(err, gateErr)
		}
	}
	return err
}

func (s *Service) fetchCanonical(ctx context.Context, href string) (store.CardDAVRemoteResource, bool, error) {
	var resource store.CardDAVRemoteResource
	var absent bool
	err := s.gate(ctx, func(ctx context.Context) error {
		var err error
		resource, absent, err = s.remote.Get(ctx, href)
		return err
	})
	return resource, absent, err
}

func (s *Service) publicationHref(collectionURL, uid string) (string, error) {
	if s == nil || s.remote == nil {
		return "", ErrUnsafeTarget
	}
	return s.remote.CreateHref(collectionURL, uid)
}

func (s *Service) operationTimeout() time.Duration {
	timeout, _ := s.remote.Limits()
	return timeout
}

// NewRemoteResource parses a vCard into the resource that a Remote returns
// from Pull and Get.
func NewRemoteResource(href, etag string, body []byte) (store.CardDAVRemoteResource, error) {
	return parseRemoteResource(href, etag, body)
}

// ErrWriteNotSent marks a Remote write that failed before its request was
// sent, so the service plans it again instead of recovering it.
var ErrWriteNotSent = errors.New("CardDAV write was not sent")

// ErrMicrosoftContactTooLarge marks a card larger than Outlook's 4 MB limit
// for one contact write.
var ErrMicrosoftContactTooLarge = errors.New("contact is too large for Outlook")

// ContactTooLargeError names the person whose card is over Outlook's limit.
// Err is the write's error, which wraps ErrMicrosoftContactTooLarge.
type ContactTooLargeError struct {
	PersonID int64
	Err      error
}

func (e *ContactTooLargeError) Error() string {
	return e.Err.Error()
}

func (e *ContactTooLargeError) Unwrap() error { return e.Err }
