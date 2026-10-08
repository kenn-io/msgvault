package carddav

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vcard"
)

var (
	ErrTruncatedSnapshot  = errors.New("CardDAV snapshot was truncated")
	ErrSyncTokenCycle     = errors.New("CardDAV sync token continuation cycle")
	ErrIncompleteMultiget = errors.New("CardDAV multiget response was incomplete")
)

const (
	maxSyncPages   = 100
	multigetBatch  = 100
	maxSyncMembers = 50_000
)

type Service struct {
	connectionName                   string
	connectionGeneration             int64
	conflictOperationMappingReadHook func()
	store                            *store.Store
	remote                           Remote
}

func NewService(st *store.Store, client *Client) *Service {
	if client == nil {
		return &Service{store: st}
	}
	return NewRemoteService(st, &davRemote{client: client})
}

type SyncOptions struct {
	Full    bool
	Trigger store.CardDAVSyncTrigger
	// OnRunStarted lets an aggregate caller attribute this exact lease without
	// racing a later sync when reading history. It runs before network work.
	OnRunStarted func(int64)
}

type SyncResult struct {
	Books       int                     `json:"books"`
	Created     int                     `json:"created"`
	Updated     int                     `json:"updated"`
	Removed     int                     `json:"removed"`
	Status      string                  `json:"status,omitempty" enum:"succeeded,partial,failed"`
	Connections []ConnectionSyncOutcome `json:"connections,omitempty"`
}

type ConnectionSyncOutcome struct {
	Connection   string `json:"connection"`
	AccountID    int64  `json:"account_id,omitzero"`
	RunID        *int64 `json:"run_id,omitempty"`
	Status       string `json:"status" enum:"succeeded,partial,failed"`
	Books        int    `json:"books"`
	Created      int    `json:"created"`
	Updated      int    `json:"updated"`
	Removed      int    `json:"removed"`
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

// Sync fetches complete network plans before entering the store's fenced
// apply transaction. A stale plan is re-fetched once; a second stale result is
// returned rather than retried blindly.
//
// The run row is finished in a deferred call so that a panic escaping the
// pull still records a terminal state. Otherwise the row would stay running
// and every later sync would be refused as active until the daemon restarts.
func (s *Service) Sync(ctx context.Context, options SyncOptions) (result SyncResult, err error) {
	if s == nil || s.store == nil || s.remote == nil {
		return SyncResult{}, errors.New("CardDAV service is not configured")
	}
	trigger := options.Trigger
	if trigger == "" {
		trigger = store.CardDAVSyncTriggerManual
	}
	accountID, err := s.scopedAccountID(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	run, err := s.store.StartCardDAVSyncRunContext(ctx, store.CardDAVSyncRunStart{
		AccountID: accountID,
		Trigger:   trigger,
		Full:      options.Full,
	})
	if err != nil {
		return SyncResult{}, err
	}
	defer func() {
		syncErr := err
		recovered := recover()
		if recovered != nil {
			syncErr = fmt.Errorf("CardDAV sync panicked: %v", recovered)
		}
		_, finishErr := s.store.FinishCardDAVSyncRunContext(
			context.WithoutCancel(ctx), run.ID, cardDAVSyncRunFinish(result, syncErr),
		)
		if recovered != nil {
			panic(recovered)
		}
		err = errors.Join(publicCardDAVSyncError(syncErr), finishErr)
	}()
	if options.OnRunStarted != nil {
		options.OnRunStarted(run.ID)
	}
	return s.sync(ctx, options)
}

func (s *Service) sync(ctx context.Context, options SyncOptions) (SyncResult, error) {
	operationCtx, cancel := context.WithTimeout(ctx, s.operationTimeout())
	defer cancel()
	if err := s.checkRetry(operationCtx); err != nil {
		return SyncResult{}, err
	}
	// Resolve ambiguous publication outcomes before interpreting remote changes.
	// Otherwise a successful write whose response timed out can be misclassified
	// as an edit/edit conflict by the pull that follows.
	recovered, err := s.recoverPendingPublications(operationCtx)
	if err != nil {
		return SyncResult{}, err
	}
	var failures []error
	_, budgetBytes := s.remote.Limits()
	budget := &Budget{remaining: budgetBytes}
	var total SyncResult
	books, err := s.scopedBooks(operationCtx)
	if err != nil {
		return total, err
	}
	for _, initial := range books {
		if !initial.IsSubscribed && !initial.IsLookupSource {
			continue
		}
		applied, err := s.syncBook(operationCtx, initial.ID, options, budget)
		if err != nil {
			bookErr := fmt.Errorf("sync CardDAV address book %d: %w", initial.ID, err)
			if isGlobalSyncFailure(operationCtx, err) {
				return total, errors.Join(append(failures, bookErr)...)
			}
			failures = append(failures, bookErr)
			continue
		}
		if applied == nil {
			continue
		}
		total.Books++
		total.Created += applied.Created
		total.Updated += applied.Updated
		total.Removed += applied.Removed
	}
	if err := s.reconcilePublications(operationCtx, recovered); err != nil {
		failures = append(failures, err)
		if isGlobalSyncFailure(operationCtx, err) {
			return total, errors.Join(failures...)
		}
	}
	if _, err := s.store.SweepResolvedCardDAVConflictsContext(operationCtx, time.Now()); err != nil {
		failures = append(failures, err)
	}
	return total, errors.Join(failures...)
}

type cardDAVSyncError struct {
	cause   error
	message string
}

func (e *cardDAVSyncError) Error() string { return e.message }
func (e *cardDAVSyncError) Unwrap() error { return e.cause }

func publicCardDAVSyncError(err error) error {
	if err == nil {
		return nil
	}
	_, message := cardDAVSyncPublicFailure(err)
	return &cardDAVSyncError{cause: err, message: message}
}

func cardDAVSyncRunFinish(result SyncResult, err error) store.CardDAVSyncRunFinish {
	finish := store.CardDAVSyncRunFinish{
		State:   store.CardDAVSyncRunSucceeded,
		Books:   int64(result.Books),
		Created: int64(result.Created),
		Updated: int64(result.Updated),
		Removed: int64(result.Removed),
	}
	if err == nil {
		return finish
	}
	finish.State = store.CardDAVSyncRunFailed
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		finish.State = store.CardDAVSyncRunCancelled
	} else if result.Books > 0 || result.Created > 0 || result.Updated > 0 || result.Removed > 0 {
		finish.State = store.CardDAVSyncRunPartial
	}
	finish.ErrorCode, finish.ErrorMessage = cardDAVSyncPublicFailure(err)
	return finish
}

func cardDAVSyncPublicFailure(err error) (string, string) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "cancelled", "CardDAV sync was cancelled."
	}
	if errors.Is(err, ErrGoogleAuthorizationRequired) {
		return "google_authorization_required", "Google Contacts authorization is required. Connect Google in CardDAV account settings."
	}
	if errors.Is(err, ErrMicrosoftAuthorizationRequired) {
		return "microsoft_authorization_required", "Microsoft contacts authorization is required. Run msgvault carddav authorize-microsoft with your account email."
	}
	if errors.Is(err, store.ErrCardDAVRetryAfter) {
		return "retry_after", "CardDAV sync is temporarily paused."
	}
	if errors.Is(err, ErrMicrosoftContactTooLarge) {
		return "microsoft_contact_too_large", operations.FixedPublicError(operations.PublicErrorMicrosoftContactTooLarge).Message
	}
	if status, ok := errors.AsType[*StatusError](err); ok {
		if status.RetryAfter > 0 {
			return "retry_after", "CardDAV sync is temporarily paused."
		}
		switch status.StatusCode {
		case http.StatusUnauthorized:
			return "authentication_failed", "CardDAV authentication failed."
		case http.StatusTooManyRequests:
			return "retry_after", "CardDAV sync is temporarily paused."
		default:
			return "upstream_failed", "CardDAV server request failed."
		}
	}
	if errors.Is(err, ErrOperationLimit) || errors.Is(err, ErrResponseLimit) {
		return "safety_limit", "CardDAV sync exceeded its safety limits."
	}
	return "sync_failed", "CardDAV sync failed."
}

func isGlobalSyncFailure(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrOperationLimit) || errors.Is(err, store.ErrCardDAVRetryAfter) || errors.Is(err, ErrGoogleAuthorizationRequired) ||
		errors.Is(err, ErrMicrosoftAuthorizationRequired) || errors.Is(err, ErrGoogleTokenUnavailable) ||
		errors.Is(err, ErrMicrosoftTokenUnavailable) {
		return true
	}
	var status *StatusError
	return errors.As(err, &status) &&
		(status.StatusCode == http.StatusUnauthorized || status.StatusCode == http.StatusTooManyRequests || status.RetryAfter > 0)
}

func (s *Service) syncBook(
	ctx context.Context, bookID int64, options SyncOptions, budget *Budget,
) (*store.CardDAVApplyResult, error) {
	state := &bookSyncState{}
	for attempt := range 2 {
		account, err := s.scopedAccount(ctx)
		if err != nil {
			return nil, err
		}
		if account == nil {
			return nil, store.ErrCardDAVStalePlan
		}
		books, err := s.scopedBooks(ctx)
		if err != nil {
			return nil, err
		}
		book, ok := findCardDAVBook(books, bookID)
		if !ok {
			return nil, store.ErrCardDAVStalePlan
		}
		if !book.IsSubscribed && !book.IsLookupSource {
			return nil, nil //nolint:nilnil // A deliberately ignored book produces no apply result and no error.
		}
		plan, err := s.fetchBookPlan(ctx, *account, book, options, budget, state)
		if err != nil {
			return nil, err
		}
		if err := s.prepareSyncConflicts(ctx, book, &plan); err != nil {
			return nil, err
		}
		applied, err := s.store.ApplyCardDAVSyncPlanContext(ctx, plan)
		if err == nil {
			return applied, nil
		}
		if !errors.Is(err, store.ErrCardDAVStalePlan) || attempt == 1 {
			return nil, err
		}
	}
	return nil, store.ErrCardDAVStalePlan
}

type bookSyncState struct {
	invalidTokenReconciled bool
}

func findCardDAVBook(books []store.CardDAVAddressBook, id int64) (store.CardDAVAddressBook, bool) {
	for _, book := range books {
		if book.ID == id {
			return book, true
		}
	}
	return store.CardDAVAddressBook{}, false
}

func (s *Service) fetchBookPlan(
	ctx context.Context, account store.CardDAVAccount, book store.CardDAVAddressBook,
	options SyncOptions, budget *Budget, state *bookSyncState,
) (store.CardDAVSyncPlan, error) {
	base := store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision,
	}
	token := book.SyncToken
	if options.Full || book.NeedsFullReconcile {
		token = ""
	}
	plan, err := s.fetchPlan(ctx, book, token, budget, state)
	if err != nil {
		return store.CardDAVSyncPlan{}, err
	}
	plan.AddressBookID = base.AddressBookID
	plan.ConnectionGeneration = base.ConnectionGeneration
	plan.SyncRevision = base.SyncRevision
	plan.CompletesFullReconcile = options.Full || book.NeedsFullReconcile
	return plan, nil
}

func (s *Service) fetchPlan(
	ctx context.Context, book store.CardDAVAddressBook, token string,
	budget *Budget, state *bookSyncState,
) (store.CardDAVSyncPlan, error) {
	pull := func(token string) (store.CardDAVSyncPlan, error) {
		var plan store.CardDAVSyncPlan
		err := s.gate(ctx, func(ctx context.Context) error {
			var err error
			plan, err = s.remote.Pull(ctx, book, token, budget)
			return err
		})
		return plan, err
	}
	plan, err := pull(token)
	if errors.Is(err, ErrInvalidSyncToken) && token != "" && !state.invalidTokenReconciled {
		state.invalidTokenReconciled = true
		return pull("")
	}
	return plan, err
}

func parseRemoteResource(href, etag string, body []byte) (store.CardDAVRemoteResource, error) {
	envelope, err := vcard.ParseResourceEnvelope(body)
	if err != nil {
		return store.CardDAVRemoteResource{}, fmt.Errorf("parse remote CardDAV vCard: %w", err)
	}
	semanticHash, err := SemanticHash(body)
	if err != nil {
		return store.CardDAVRemoteResource{}, err
	}
	resource := store.CardDAVRemoteResource{
		Href: href, RemoteETag: etag, RemoteBody: append([]byte(nil), body...),
		SemanticHash: semanticHash,
	}
	for _, occurrence := range envelope.PropertyTree {
		property := occurrence.Property
		identity := cardDAVVCardIdentity(occurrence)
		switch strings.ToUpper(property.Name) {
		case "UID":
			if resource.RemoteUID == "" {
				resource.RemoteUID = strings.TrimSpace(property.RawValue)
			}
		case "FN":
			if resource.DisplayName == "" {
				value, err := cardDAVPropertyValue(envelope.RenderMetadata.StoredVersion, property)
				if err != nil {
					return store.CardDAVRemoteResource{}, fmt.Errorf("decode CardDAV FN: %w", err)
				}
				resource.DisplayName = strings.TrimSpace(value)
				resource.DisplayNameIdentity = identity
			}
		case "EMAIL":
			value, err := cardDAVPropertyValue(envelope.RenderMetadata.StoredVersion, property)
			if err != nil {
				return store.CardDAVRemoteResource{}, fmt.Errorf("decode CardDAV EMAIL: %w", err)
			}
			value = strings.TrimSpace(trimPrefixFold(value, "mailto:"))
			if value != "" {
				resource.Emails = append(resource.Emails, value)
				resource.EmailIdentities = append(resource.EmailIdentities, identity)
			}
		case "TEL":
			value, err := cardDAVPropertyValue(envelope.RenderMetadata.StoredVersion, property)
			if err != nil {
				return store.CardDAVRemoteResource{}, fmt.Errorf("decode CardDAV TEL: %w", err)
			}
			value = strings.TrimSpace(trimPrefixFold(value, "tel:"))
			if value != "" {
				resource.Phones = append(resource.Phones, value)
				resource.PhoneIdentities = append(resource.PhoneIdentities, identity)
			}
		}
	}
	return resource, nil
}

func cardDAVPropertyValue(version vcard.Version, property vcard.Property) (string, error) {
	valueType := ""
	for _, parameter := range property.ParametersNamed("VALUE") {
		if len(parameter.Values) > 0 {
			valueType = strings.ToLower(strings.TrimSpace(parameter.Values[0].Decoded))
			break
		}
	}
	name := strings.ToUpper(property.Name)
	isText := valueType == "text" || valueType == "" &&
		(name != "TEL" || version != vcard.Version40)
	if !isText {
		return property.RawValue, nil
	}
	return vcard.UnescapeText(property.RawValue)
}

func cardDAVVCardIdentity(occurrence vcard.PropertyOccurrence) store.VCardIdentity {
	identity := store.VCardIdentity{
		Property: strings.ToUpper(occurrence.Property.Name),
		PropID:   occurrence.Identity.PropID,
		PID:      append([]string(nil), occurrence.Identity.PID...),
		AltID:    occurrence.Identity.AltID,
	}
	if occurrence.Identity.Group != "" {
		group := occurrence.Identity.Group
		identity.Group = &group
	}
	return identity
}

func trimPrefixFold(value, prefix string) string {
	if len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix) {
		return value[len(prefix):]
	}
	return value
}

// SyncCollectionBody builds the RFC 6578 level-1 incremental request.
func SyncCollectionBody(token string) ([]byte, error) {
	var body bytes.Buffer
	encoder := xml.NewEncoder(&body)
	root := xml.StartElement{Name: xml.Name{Space: davNamespace, Local: "sync-collection"}}
	if err := encoder.EncodeToken(root); err != nil {
		return nil, fmt.Errorf("encode CardDAV sync root: %w", err)
	}
	fields := []struct {
		name, text string
	}{{"sync-token", token}, {"sync-level", "1"}}
	for _, value := range fields {
		start := xml.StartElement{Name: xml.Name{Space: davNamespace, Local: value.name}}
		if err := encoder.EncodeToken(start); err != nil {
			return nil, fmt.Errorf("encode CardDAV sync field %s: %w", value.name, err)
		}
		if err := encoder.EncodeToken(xml.CharData(value.text)); err != nil {
			return nil, fmt.Errorf("encode CardDAV sync value %s: %w", value.name, err)
		}
		if err := encoder.EncodeToken(start.End()); err != nil {
			return nil, fmt.Errorf("close CardDAV sync field %s: %w", value.name, err)
		}
	}
	prop := xml.StartElement{Name: xml.Name{Space: davNamespace, Local: "prop"}}
	if err := encoder.EncodeToken(prop); err != nil {
		return nil, fmt.Errorf("encode CardDAV sync properties: %w", err)
	}
	if err := encodeEmptyElement(encoder, xml.Name{Space: davNamespace, Local: "getetag"}); err != nil {
		return nil, err
	}
	if err := encoder.EncodeToken(prop.End()); err != nil {
		return nil, fmt.Errorf("close CardDAV sync properties: %w", err)
	}
	if err := encoder.EncodeToken(root.End()); err != nil {
		return nil, fmt.Errorf("close CardDAV sync root: %w", err)
	}
	if err := encoder.Flush(); err != nil {
		return nil, fmt.Errorf("flush CardDAV sync XML: %w", err)
	}
	return body.Bytes(), nil
}
