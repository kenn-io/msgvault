package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gmail"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/msgraph"
	"go.kenn.io/msgvault/internal/msmail"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/provideridentity"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	msgsync "go.kenn.io/msgvault/internal/sync"
)

func rapidEmailPollingEnabled(cfg *config.Config, identifier string) bool {
	if cfg == nil {
		return false
	}
	for _, account := range cfg.Accounts {
		if account.Enabled && account.Email == identifier && account.PollInterval > 0 {
			return true
		}
	}
	return false
}

// Every pass resolves its current source inside the scheduler's acquired gate.
// Display names, archive aliases and token-first registration are not selectors.
func rapidEmailSource(st *store.Store, identifier string) (*store.Source, error) {
	rows, err := st.GetSourcesByIdentifierOrDisplayName(identifier)
	if err != nil {
		return nil, err
	}
	var selected *store.Source
	for _, row := range rows {
		if row.Identifier != identifier || row.MergedIntoSourceID != 0 {
			continue
		}
		switch row.SourceType {
		case sourceTypeGmail, sourceTypeIMAP, sourceTypeMSMail:
		default:
			continue
		}
		if selected != nil {
			return nil, &scheduler.AcquisitionError{Outcome: scheduler.AcquisitionFailed, Reason: scheduler.AcquisitionCanonicalTarget}
		}
		selected = row
	}
	if selected == nil {
		return nil, &scheduler.AcquisitionError{Outcome: scheduler.AcquisitionFailed, Reason: scheduler.AcquisitionCanonicalTarget}
	}
	return selected, nil
}

func validateRapidIMAP(src *store.Source, cfg *config.Config) error {
	if src == nil || !src.SyncConfig.Valid {
		return errors.New("rapid IMAP requires saved canonical connection settings")
	}
	settings, err := imaplib.ConfigFromJSON(src.SyncConfig.String)
	if err != nil || settings.Identifier() != src.Identifier {
		return errors.New("rapid IMAP connection settings do not match the canonical account")
	}
	if len(settings.Folders) > 0 || len(parseFolderFilter(cfg.Sync.IMAPSkipFolders[src.Identifier])) > 0 || syncAfter != "" || syncBefore != "" {
		return &scheduler.AcquisitionError{Outcome: scheduler.AcquisitionFailed, Reason: scheduler.AcquisitionFilteredCoverage}
	}
	return nil
}

func rapidRetryFloor(client any) time.Time {
	if limited, ok := client.(interface{ RetryNotBefore() time.Time }); ok {
		return limited.RetryNotBefore()
	}
	return time.Time{}
}

func rapidAcquisitionResult(ctx context.Context, err error, incomplete bool, floor time.Time) error {
	var prior *scheduler.AcquisitionError
	if errors.As(err, &prior) && prior.NotBefore.After(floor) {
		floor = prior.NotBefore
	}
	var limited interface{ RetryNotBefore() time.Time }
	if errors.As(err, &limited) && limited.RetryNotBefore().After(floor) {
		floor = limited.RetryNotBefore()
	}
	if err == nil && !incomplete && !floor.After(time.Now()) && ctx.Err() == nil && !scheduledSyncYielded(ctx) {
		return nil
	}
	outcome := scheduler.AcquisitionFailed
	if prior != nil {
		outcome = prior.Outcome
	}
	if err == nil && (incomplete || floor.After(time.Now())) {
		outcome = scheduler.AcquisitionIncomplete
	}
	if ctx.Err() != nil || scheduledSyncYielded(ctx) {
		outcome = scheduler.AcquisitionCancelled
		err = errors.Join(err, ctx.Err())
	}
	var credential *provideridentity.GmailCredentialError
	var status *gmail.StatusError
	credentials := errors.As(err, &credential) || errors.Is(err, os.ErrNotExist) || errors.Is(err, msgraph.ErrUnauthorized) || errors.Is(err, msgraph.ErrForbidden) || errors.Is(err, microsoft.ErrGraphMailboxMismatch)
	if prior != nil && prior.Credentials {
		credentials = true
	}
	if errors.As(err, &status) && (status.StatusCode == 401 || status.StatusCode == 403) {
		credentials = true
	}
	reason := scheduler.AcquisitionReasonUnknown
	if prior != nil {
		reason = prior.Reason
	}
	return &scheduler.AcquisitionError{Outcome: outcome, Reason: reason, NotBefore: floor, Credentials: credentials, Err: err}
}

func rapidRefreshCache(ctx context.Context, identifier string, state *invocation) {
	if err := rebuildCacheAfterScheduledSourceRun(context.WithoutCancel(ctx), identifier); err != nil {
		state.logger.Warn("email archive refresh could not be queued; scheduled indexing remains available")
	}
}

func runRapidEmailSync(ctx context.Context, identifier string, st *store.Store, getOAuthMgr func(context.Context, string) (*oauth.Manager, error), state *invocation) error {
	src, err := rapidEmailSource(st, identifier)
	if err != nil {
		return rapidAcquisitionResult(ctx, err, false, time.Time{})
	}
	switch src.SourceType {
	case sourceTypeGmail:
		if getOAuthMgr == nil && state.cfg.OAuth.ServiceAccountKeyFor(sourceOAuthApp(src)) == "" {
			err = provideridentity.NewGmailCredentialError(provideridentity.GmailOAuthConfiguration, errors.New("Gmail OAuth configuration is unavailable"))
			return rapidAcquisitionResult(ctx, err, false, time.Time{})
		}
		client, _, err := newDaemonGmailClient(ctx, src.Identifier, src, getOAuthMgr, state, oauth.ScopesGmailReadonly)
		if err != nil {
			return rapidAcquisitionResult(ctx, err, false, time.Time{})
		}
		defer func() { _ = client.Close() }()
		if deferred, ok := client.(interface{ DeferProviderRetries() }); ok {
			deferred.DeferProviderRetries()
		}
		return runRapidGmailSource(ctx, src, st, client, state)
	case sourceTypeMSMail:
		token, err := newGraphMailManager(state).TokenSource(ctx, src.Identifier)
		if err != nil {
			return rapidAcquisitionResult(ctx, &scheduler.AcquisitionError{Outcome: scheduler.AcquisitionFailed, Credentials: true, Err: err}, false, time.Time{})
		}
		client := msmail.NewClient(msmail.GraphBaseURL, token, msmailQPS)
		client.DeferProviderRetries()
		return runRapidGraphSource(ctx, src, st, client, state)
	case sourceTypeIMAP:
		if err := validateRapidIMAP(src, state.cfg); err != nil {
			return rapidAcquisitionResult(ctx, err, false, time.Time{})
		}
		summary, err := runScheduledIMAPSync(ctx, src, st, state)
		defer rapidRefreshCache(ctx, identifier, state)
		return rapidAcquisitionResult(ctx, err, summary == nil || summary.Errors > 0, time.Time{})
	}
	return rapidAcquisitionResult(ctx, errors.New("rapid email source is unavailable"), false, time.Time{})
}

func runRapidGmailSource(ctx context.Context, src *store.Source, st *store.Store, client gmail.API, state *invocation) (err error) {
	incomplete := true
	defer func() { err = rapidAcquisitionResult(ctx, err, incomplete, rapidRetryFloor(client)) }()
	profile, err := client.GetProfile(ctx)
	if err != nil {
		return err
	}
	if profile == nil {
		return errors.New("authenticated Gmail profile is unavailable")
	}
	if _, err = provideridentity.GmailProfileEvidence(src, profile.EmailAddress); err != nil {
		return &scheduler.AcquisitionError{Outcome: scheduler.AcquisitionFailed, Credentials: true, Err: err}
	}
	// Profile evidence must precede migrations, identity confirmation and sync runs.
	defer rapidRefreshCache(ctx, src.Identifier, state)
	confirmDefaultIdentity(io.Discard, st, src.ID, src.Identifier, src.Identifier, "account-identifier", state.logger)
	if err = runPostSourceCreateMigrationsForInvocation(st, state); err != nil {
		return err
	}
	opts := msgsync.DefaultOptions()
	opts.AttachmentsDir = state.cfg.AttachmentsDir()
	syncer := newMessageSyncer(client, st, opts, state).WithLogger(state.logger)
	var summary *gmail.SyncSummary
	if !src.SyncCursor.Valid || src.SyncCursor.String == "" {
		summary, err = syncer.RecoverExpiredHistory(ctx, src)
	} else {
		summary, err = syncer.IncrementalWithHistoryRecovery(ctx, src, nil)
	}
	if err != nil {
		return err
	}
	incomplete = summary == nil || summary.Errors > 0
	debt, err := st.ListUnresolvedSyncItemErrors(ctx, src.ID)
	if err != nil {
		return err
	}
	incomplete = incomplete || len(debt) > 0
	checkpoint, err := st.GetLatestCheckpointedSyncContext(ctx, src.ID)
	if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
		return err
	}
	incomplete = incomplete || checkpoint != nil
	return nil
}

func runRapidGraphSource(ctx context.Context, src *store.Source, st *store.Store, client *msmail.Client, state *invocation) (err error) {
	incomplete := true
	defer func() { err = rapidAcquisitionResult(ctx, err, incomplete, client.RetryNotBefore()) }()
	if err = microsoft.VerifyGraphMailbox(ctx, client.Client, src.Identifier); err != nil {
		return err
	}
	defer rapidRefreshCache(ctx, src.Identifier, state)
	confirmDefaultIdentity(io.Discard, st, src.ID, src.Identifier, src.Identifier, "account-identifier", state.logger)
	if err = runPostSourceCreateMigrationsForInvocation(st, state); err != nil {
		return err
	}
	summary, err := msmail.ImportSource(ctx, st, client, src, msmail.Options{Email: src.Identifier, AttachmentsDir: state.cfg.AttachmentsDir()}, state.logger)
	if err != nil {
		return err
	}
	incomplete = summary == nil || summary.Errors > 0 || summary.Incomplete
	return nil
}
