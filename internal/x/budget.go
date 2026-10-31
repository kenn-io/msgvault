package x

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

var (
	ErrConnectVerificationRequired = errors.New("X account verification is connect-only; use the separately disclosed verification read")
	ErrSendAdmissionRequired       = errors.New("X replies require explicit send admission")
)

// ReadScope pins the lifetime cap for every archive read and retry. Empty JobID
// is used for ordinary timeline, refresh and explicit-adoption reads.
type ReadScope struct{ JobID string }

type admittedAPI struct {
	st             *store.Store
	sourceID       int64
	api            API
	policy         config.XAccountConfig
	credentials    Credentials
	scope          ReadScope
	now            func() time.Time
	wait           func(context.Context, time.Duration) error
	mu             sync.Mutex
	jobFingerprint string
	blocked        error
	beforeLookup   func(context.Context, []string) error
}

// NewAdmittedAPI wraps a single-request raw API. The caller retains source
// execution ownership and supplies its credential object's HTTPClient to the
// raw client. Reads reserve before I/O; this wrapper never sends or settles.
func NewAdmittedAPI(st *store.Store, sourceID int64, api API, policy config.XAccountConfig, credentials Credentials, scope ReadScope) API {
	return &admittedAPI{st: st, sourceID: sourceID, api: api, policy: policy, credentials: credentials, scope: scope, now: time.Now, wait: waitXRead}
}

var _ API = (*admittedAPI)(nil)

func (a *admittedAPI) Me(context.Context) (User, error) {
	return User{}, ErrConnectVerificationRequired
}
func (a *admittedAPI) CreateReply(context.Context, string, string) (string, error) {
	return "", ErrSendAdmissionRequired
}

func (a *admittedAPI) Timeline(ctx context.Context, request TimelineRequest) (Page, error) {
	if ValidateID(request.OwnerID) != nil || request.OwnerID != a.policy.UserID || request.MaxResults < 5 || request.MaxResults > 100 || (request.SinceID != "" && (ValidateID(request.SinceID) != nil || !request.After.IsZero())) || (!request.After.IsZero() && !request.Before.IsZero() && !request.After.Before(request.Before)) {
		return Page{}, store.ErrXRequestCost
	}
	kind := store.XRequestTimeline
	if request.Mentions {
		kind = store.XRequestMentions
	}
	cost := store.XRequestCost{Kind: kind, RequestedResults: request.MaxResults, After: request.After, Before: request.Before}
	return a.read(ctx, cost, func(size int) (Page, error) { request.MaxResults = size; return a.api.Timeline(ctx, request) })
}
func (a *admittedAPI) SearchAll(ctx context.Context, request SearchRequest) (Page, error) {
	if strings.TrimSpace(request.Query) == "" || len(request.Query) > 4096 || request.MaxResults < 10 || request.MaxResults > 500 || (!request.After.IsZero() && !request.Before.IsZero() && !request.After.Before(request.Before)) {
		return Page{}, store.ErrXRequestCost
	}
	cost := store.XRequestCost{Kind: store.XRequestSearch, RequestedResults: request.MaxResults, Query: request.Query, After: request.After, Before: request.Before}
	return a.read(ctx, cost, func(size int) (Page, error) { request.MaxResults = size; return a.api.SearchAll(ctx, request) })
}
func (a *admittedAPI) Lookup(ctx context.Context, requested []string) (Page, error) {
	ids := slices.Clone(requested)
	if len(ids) < 1 || len(ids) > 100 {
		return Page{}, store.ErrXRequestCost
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if ValidateID(id) != nil || seen[id] {
			return Page{}, store.ErrXRequestCost
		}
		seen[id] = true
	}
	page, err := a.read(ctx, store.XRequestCost{Kind: store.XRequestLookup, RequestedResults: len(ids)}, func(size int) (Page, error) {
		selected := ids[:size]
		if a.beforeLookup != nil {
			if err := a.beforeLookup(ctx, selected); err != nil {
				return Page{}, err
			}
		}
		return a.api.Lookup(ctx, selected)
	})
	if err == nil {
		page.RequestedIDs = slices.Clone(ids[:page.RequestedResults])
	}
	return page, err
}

func (a *admittedAPI) read(ctx context.Context, spec store.XRequestCost, request func(int) (Page, error)) (Page, error) {
	// One credential object belongs to one execution owner. Serialize its reads
	// so a concurrent Prepare cannot change the bearer after this snapshot.
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.blocked != nil {
		return Page{}, a.blocked
	}
	if a.st == nil || a.api == nil || a.credentials == nil {
		return Page{}, store.ErrXBudgetIdentity
	}
	if err := a.policy.Validate(a.policy.UserID); err != nil {
		return Page{}, err
	}
	if a.policy.MonthlyBudgetMicros == 0 {
		return Page{}, store.ErrXBudgetExceeded
	}
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}
	if err := a.credentialPreflight(ctx); err != nil {
		return Page{}, err
	}
	snapshot, err := a.credentials.Prepare(ctx)
	if err != nil {
		return Page{}, a.credentialFailure(ctx, err)
	}
	refreshed := false
	retries := 0
	waited := time.Duration(0)
	for {
		if snapshot.OwnerID != a.policy.UserID {
			return Page{}, store.ErrXBudgetIdentity
		}
		cost := spec
		cost.OwnerID = snapshot.OwnerID
		cost.Generation = snapshot.Generation
		cost.GrantedScopes = slices.Clone(snapshot.GrantedScopes)
		cost.MonthlyBudgetMicros = a.policy.MonthlyBudgetMicros
		cost.PostPriceMicros = a.policy.PostReadPriceMicros
		cost.UserPriceMicros = a.policy.UserReadPriceMicros
		if (cost.Kind == store.XRequestTimeline || cost.Kind == store.XRequestMentions) && a.policy.OwnedReads && !a.policy.PostReadPriceOverride {
			cost.PostPriceMicros = 1000
		}
		cost.At = a.now().UTC()
		cost.JobID = a.scope.JobID
		if cost.JobID != "" {
			if a.jobFingerprint == "" {
				state, err := a.st.LoadXControlContext(ctx, a.sourceID)
				if err != nil {
					return Page{}, err
				}
				for _, job := range state.Jobs {
					if job.ID == cost.JobID {
						a.jobFingerprint = job.Fingerprint
						break
					}
				}
				if a.jobFingerprint == "" {
					return Page{}, store.ErrXArchiveScope
				}
			}
			cost.JobFingerprint = a.jobFingerprint
		}
		reservation, err := a.st.ReserveXRequestContext(ctx, a.sourceID, cost)
		if err != nil {
			return Page{}, err
		}
		page, err := request(reservation.MaxPosts)
		if err == nil {
			if len(page.Posts) > reservation.MaxPosts || len(page.Users) > reservation.MaxUsers || page.PostCount > reservation.MaxPosts || page.UserCount > reservation.MaxUsers {
				err = ErrReadModelExceeded
			} else if page.PostCount != len(page.Posts) || page.UserCount != len(page.Users) {
				err = ErrResponseShape
			} else {
				page.ReservationID = reservation.ID
				page.FetchedAt = a.now().UTC()
				page.RequestedResults = reservation.MaxPosts
				return page, nil
			}
		}
		// No failure or cancellation can release a committed reservation.
		if errors.Is(err, ErrReadModelExceeded) {
			// Retain execution ownership while persisting an observed model failure,
			// even when the requesting caller cancelled after the response arrived.
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			recordErr := a.st.RecordXModelErrorContext(cleanup, a.sourceID, snapshot.OwnerID, reservation.ID)
			cancel()
			if recordErr != nil {
				a.blocked = errors.Join(err, recordErr)
				return Page{}, a.blocked
			}
			return Page{}, err
		}
		if ctx.Err() != nil {
			return Page{}, ctx.Err()
		}
		var apiError *APIError
		if errors.As(err, &apiError) && apiError.StatusCode == 401 {
			if refreshed {
				recordErr := a.st.RecordXCredentialErrorContext(ctx, a.sourceID, snapshot.OwnerID, snapshot.Generation, "unauthorized")
				return Page{}, errors.Join(err, recordErr)
			}
			refreshed = true
			if preflightErr := a.credentialPreflight(ctx); preflightErr != nil {
				return Page{}, preflightErr
			}
			snapshot, err = a.credentials.Refresh(ctx, snapshot.Generation)
			if err != nil {
				return Page{}, a.credentialFailure(ctx, err)
			}
			continue
		}
		retryable := errors.Is(err, ErrTransport) || (apiError != nil && (apiError.StatusCode == 429 || (apiError.StatusCode >= 500 && apiError.StatusCode <= 599)))
		if !retryable || retries >= 2 {
			return Page{}, err
		}
		delay, delayErr := xReadRetryDelay(apiError, retries, a.now().UTC())
		if delayErr != nil {
			return Page{}, errors.Join(err, delayErr)
		}
		if delay > 15*time.Minute-waited {
			return Page{}, errors.Join(err, ErrReadRetryDelay)
		}
		waited += delay
		if err := a.wait(ctx, delay); err != nil {
			return Page{}, err
		}
		retries++
		if preflightErr := a.credentialPreflight(ctx); preflightErr != nil {
			return Page{}, preflightErr
		}
		snapshot, err = a.credentials.Prepare(ctx)
		if err != nil {
			return Page{}, a.credentialFailure(ctx, err)
		}
	}
}

func (a *admittedAPI) credentialFailure(ctx context.Context, err error) error {
	var invalid *InvalidGrantError
	if errors.As(err, &invalid) {
		return errors.Join(err, a.st.RecordXCredentialErrorContext(ctx, a.sourceID, invalid.OwnerID, invalid.Generation, "invalid_grant"))
	}
	return err
}

// A durable disable is cleared only by explicit verified registration. This
// check MUST precede Prepare/Refresh: an expiring token must not increment its
// generation through a persisted failure merely because wall clock advanced.
func (a *admittedAPI) credentialPreflight(ctx context.Context) error {
	state, err := a.st.LoadXControlContext(ctx, a.sourceID)
	if err != nil {
		return err
	}
	if state.OwnerID != a.policy.UserID {
		return store.ErrXBudgetIdentity
	}
	if state.ModelError != "" {
		return store.ErrXModelError
	}
	if state.CredentialError != "" {
		return store.ErrXBudgetIdentity
	}
	return nil
}
