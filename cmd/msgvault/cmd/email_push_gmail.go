package cmd

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"sync/atomic"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/emailpush"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/provideridentity"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

// Gmail listeners share the runtime's immutable source bindings and strict wake
// callback. A listener never replaces the scheduler's shared wake callback.

type gmailPushClient interface {
	GetProfile(ctx context.Context) (*gmail.Profile, error)
	Watch(ctx context.Context, topic string) (*gmail.WatchResult, error)
	Close() error
}

type gmailPushError struct {
	operation                   string
	cause                       error
	terminal, sourceUnavailable bool
}

func (e *gmailPushError) Error() string {
	text := e.operation + " failed; polling remains enabled"
	if credential, ok := errors.AsType[*provideridentity.GmailCredentialError](e.cause); ok {
		if remediation := credential.Remediation(); remediation != "" {
			return text + ": " + remediation
		}
	}
	return text
}
func (e *gmailPushError) Unwrap() error { return e.cause }
func (e *gmailPushError) Is(target error) bool {
	return e.sourceUnavailable && target == emailpush.ErrSourceUnavailable
}

func gmailPushPermanentFailure(err error) bool {
	if errors.Is(err, emailpush.ErrSourceUnavailable) {
		return true
	}
	if failure, ok := errors.AsType[*gmailPushError](err); ok && failure.terminal {
		return true
	}
	if _, ok := errors.AsType[*provideridentity.GmailCredentialError](err); ok {
		return true
	}
	if _, ok := errors.AsType[*emailpush.ValidationError](err); ok {
		return true
	}
	if _, ok := errors.AsType[*gmail.ThrottledError](err); ok {
		return false
	}
	code := 0
	if status, ok := errors.AsType[*gmail.StatusError](err); ok {
		code = status.StatusCode
	}
	if status, ok := errors.AsType[*emailpush.HTTPError](err); ok {
		code = status.StatusCode
	}
	return code >= http.StatusBadRequest && code < http.StatusInternalServerError && code != http.StatusRequestTimeout && code != http.StatusTooManyRequests
}

func gmailPushScheduleEnabled(cfg *config.Config, identifier string) bool {
	if cfg == nil {
		return false
	}
	matches := 0
	for _, account := range cfg.Accounts {
		if account.Email != identifier {
			continue
		}
		if !account.Enabled || account.Schedule == "" {
			return false
		}
		matches++
	}
	return matches == 1
}

// newGmailPushListener captures one source incarnation. Stop and join Run before
// closing its returned client. The parent runtime installs the union strict wake
// callback once; WakeAccountAndWait here only requests that existing exact-source
// completion path. No alias, display-name or token-first source creation occurs.
func newGmailPushListener(
	ctx context.Context, entry config.GmailPushConfig, st *store.Store, sched *scheduler.Scheduler,
	state *invocation, getOAuthMgr func(context.Context, string) (*oauth.Manager, error),
) (*emailpush.GmailListener, io.Closer, error) {
	if err := entry.Validate(); err != nil {
		return nil, nil, err
	}
	if st == nil || sched == nil || state == nil || state.cfg == nil || getOAuthMgr == nil {
		return nil, nil, &gmailPushError{operation: "Gmail push configuration", terminal: true}
	}
	source, token, err := st.BindEmailPushSource(ctx, entry.SourceID)
	if err != nil {
		definitive := errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrSourceNotFound) || errors.Is(err, store.ErrSourceRetired)
		return nil, nil, &gmailPushError{operation: "Gmail source binding", cause: err, terminal: definitive, sourceUnavailable: definitive}
	}
	if source == nil || source.SourceType != sourceTypeGmail {
		return nil, nil, &gmailPushError{operation: "Gmail source binding", terminal: true, sourceUnavailable: true}
	}
	binding := emailPushBinding{sourceID: source.ID, sourceType: source.SourceType, identifier: source.Identifier, token: token}
	return newGmailPushListenerForBinding(ctx, entry, binding, st, sched, state, getOAuthMgr)
}

// Reconnects reuse the startup token, so replacing a source while credentials or
// providers are unavailable cannot transfer pending notifications to a new row.
func newGmailPushListenerForBinding(
	ctx context.Context, entry config.GmailPushConfig, binding emailPushBinding,
	st *store.Store, sched *scheduler.Scheduler, state *invocation,
	getOAuthMgr func(context.Context, string) (*oauth.Manager, error),
) (*emailpush.GmailListener, io.Closer, error) {
	if binding.sourceID != entry.SourceID || binding.sourceType != sourceTypeGmail {
		return nil, nil, &gmailPushError{operation: "Gmail source binding", terminal: true, sourceUnavailable: true}
	}
	current := func(ctx context.Context) (*store.Source, error) {
		if !gmailPushScheduleEnabled(state.cfg, binding.identifier) || !sched.IsScheduled(binding.identifier) {
			return nil, errEmailPushBinding
		}
		return binding.current(ctx, st)
	}
	source, err := current(ctx)
	if err != nil {
		definitive := errors.Is(err, errEmailPushBinding)
		return nil, nil, &gmailPushError{operation: "Gmail source binding", cause: err, terminal: definitive, sourceUnavailable: definitive}
	}
	apiClient, serviceAccount, err := newDaemonGmailClient(ctx, source.Identifier, source, getOAuthMgr, state, oauth.Scopes)
	if err != nil {
		return nil, nil, &gmailPushError{operation: "Gmail source credentials", cause: err, terminal: gmailPushPermanentFailure(err)}
	}
	client, ok := apiClient.(gmailPushClient)
	if !ok {
		_ = apiClient.Close()
		return nil, nil, &gmailPushError{operation: "Gmail watch capability", terminal: true}
	}
	subscriber, err := emailpush.NewPubSubFromCredentials(ctx, entry.CredentialsFile, entry.Subscription, entry.Topic)
	if err != nil {
		_ = client.Close()
		return nil, nil, &gmailPushError{operation: "Gmail subscriber credentials", cause: err, terminal: gmailPushPermanentFailure(err)}
	}
	var report func(error)
	if state.logger != nil {
		report = func(err error) { state.logger.Warn("Gmail push is retrying; polling remains enabled", "error", err) }
	}
	listener, err := newBoundGmailPushListener(source, entry, client, subscriber, current, sched.WakeAccountAndWait, serviceAccount, report)
	if err != nil {
		_ = client.Close()
		return nil, nil, err
	}
	return listener, client, nil
}

// newBoundGmailPushListener keeps the external credential client and exact-row
// loader injectable without weakening production's Store incarnation token.
func newBoundGmailPushListener(
	initial *store.Source, entry config.GmailPushConfig, client gmailPushClient, subscriber emailpush.GmailSubscriber,
	current func(context.Context) (*store.Source, error), wake func(context.Context, string) error,
	serviceAccount bool, onError func(error),
) (*emailpush.GmailListener, error) {
	if err := entry.Validate(); err != nil {
		return nil, err
	}
	if initial == nil || initial.ID != entry.SourceID || initial.SourceType != "gmail" || initial.HistoryOnly || initial.MergedIntoSourceID != 0 || client == nil || subscriber == nil || current == nil || wake == nil {
		return nil, &gmailPushError{operation: "Gmail source binding", terminal: true, sourceUnavailable: true}
	}
	sourceID, identifier := initial.ID, initial.Identifier
	var verified, rejected atomic.Bool
	bound := func(ctx context.Context) (*store.Source, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		source, err := current(ctx)
		if err != nil {
			definitive := errors.Is(err, errEmailPushBinding)
			return nil, &gmailPushError{operation: "Gmail source binding", cause: err, terminal: definitive, sourceUnavailable: definitive}
		}
		if rejected.Load() || source == nil || source.ID != sourceID || source.SourceType != "gmail" || source.Identifier != identifier || source.HistoryOnly || source.MergedIntoSourceID != 0 {
			return nil, &gmailPushError{operation: "Gmail source binding", cause: err, terminal: true, sourceUnavailable: true}
		}
		return source, nil
	}
	listener := &emailpush.GmailListener{Subscriber: subscriber, OnError: onError}
	listener.ValidateSource = func(ctx context.Context) error { _, err := bound(ctx); return err }
	listener.Watch = func(ctx context.Context) (emailpush.GmailWatch, error) {
		source, err := bound(ctx)
		if err != nil {
			return emailpush.GmailWatch{}, err
		}
		profile, err := client.GetProfile(ctx)
		if err != nil {
			classified := provideridentity.ClassifyGmailProfileError(err, serviceAccount)
			permanent := gmailPushPermanentFailure(classified)
			if permanent {
				rejected.Store(true)
			}
			return emailpush.GmailWatch{}, &gmailPushError{operation: "Gmail profile verification", cause: classified, terminal: permanent, sourceUnavailable: permanent}
		}
		if profile == nil {
			return emailpush.GmailWatch{}, &gmailPushError{operation: "Gmail profile verification"}
		}
		if _, err := provideridentity.GmailProfileEvidence(source, profile.EmailAddress); err != nil {
			rejected.Store(true)
			return emailpush.GmailWatch{}, &gmailPushError{operation: "Gmail profile binding", cause: err, terminal: true, sourceUnavailable: true}
		}
		if _, err := bound(ctx); err != nil {
			return emailpush.GmailWatch{}, err
		}
		result, err := client.Watch(ctx, entry.Topic)
		if err != nil {
			permanent := gmailPushPermanentFailure(err)
			if permanent {
				rejected.Store(true)
			}
			return emailpush.GmailWatch{}, &gmailPushError{operation: "Gmail watch setup (check topic and publisher permission)", cause: err, terminal: permanent, sourceUnavailable: permanent}
		}
		if result == nil {
			return emailpush.GmailWatch{}, &gmailPushError{operation: "Gmail watch response"}
		}
		if _, err := bound(ctx); err != nil {
			return emailpush.GmailWatch{}, err
		}
		verified.Store(true)
		return emailpush.GmailWatch{EmailAddress: profile.EmailAddress, Expiration: result.Expiration}, nil
	}
	listener.Reconcile = func(ctx context.Context) error {
		if !verified.Load() {
			return &gmailPushError{operation: "Gmail profile binding", terminal: true, sourceUnavailable: true}
		}
		if _, err := bound(ctx); err != nil {
			return err
		}
		if err := wake(ctx, identifier); err != nil {
			return &gmailPushError{operation: "Gmail source reconciliation", cause: err, terminal: gmailPushPermanentFailure(err)}
		}
		_, err := bound(ctx)
		return err
	}
	return listener, nil
}

// verifyGmailPushIngestionProfile runs on the exact client selected for the
// ingestion pass, before the syncer can create a run or mutate cursor/retry debt.
// The syncer's own identity refresh remains best-effort for ordinary syncs.
func verifyGmailPushIngestionProfile(ctx context.Context, source *store.Source, client interface {
	GetProfile(ctx context.Context) (*gmail.Profile, error)
}, serviceAccount bool) error {
	profile, err := client.GetProfile(ctx)
	if err != nil {
		classified := provideridentity.ClassifyGmailProfileError(err, serviceAccount)
		permanent := gmailPushPermanentFailure(classified)
		return &gmailPushError{operation: "Gmail ingestion profile verification", cause: classified, terminal: permanent, sourceUnavailable: permanent}
	}
	if profile == nil {
		return &gmailPushError{operation: "Gmail ingestion profile verification"}
	}
	if _, err := provideridentity.GmailProfileEvidence(source, profile.EmailAddress); err != nil {
		return &gmailPushError{operation: "Gmail ingestion profile binding", cause: err, terminal: true, sourceUnavailable: true}
	}
	return ctx.Err()
}

// runGmailPushListener owns the session and client lifetime. Transient setup
// failures retry with bounded jitter; terminal failures leave polling enabled.
func runGmailPushListener(ctx context.Context, entry config.GmailPushConfig, binding emailPushBinding,
	st *store.Store, sched *scheduler.Scheduler, state *invocation,
	getOAuthMgr func(context.Context, string) (*oauth.Manager, error),
) {
	for attempt := 0; ctx.Err() == nil; attempt++ {
		listener, closer, err := newGmailPushListenerForBinding(ctx, entry, binding, st, sched, state, getOAuthMgr)
		if err == nil {
			err = listener.Run(ctx)
			// Run joins its renewal and Pull goroutines before the client closes.
			_ = closer.Close()
		}
		if ctx.Err() != nil {
			return
		}
		if gmailPushPermanentFailure(err) {
			state.logger.Warn("Gmail push listener stopped; polling remains enabled", "source_id", binding.sourceID, "reason", "source, authentication or configuration unavailable")
			return
		}
		delay := imapPushRetryDelay(attempt)
		state.logger.Warn("Gmail push listener reconnecting; polling remains enabled", "source_id", binding.sourceID, "retry_in", delay)
		if !waitEmailPushRetry(ctx, delay) {
			return
		}
	}
}
