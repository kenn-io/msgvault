package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gmail"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/provideridentity"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	messagesync "go.kenn.io/msgvault/internal/sync"
	"go.kenn.io/msgvault/internal/syncerr"
)

var (
	errEmailPushBinding    = errors.New("email push requires an active exact source and enabled canonical account schedule")
	errEmailPushIncomplete = errors.New("email push reconciliation did not complete")
	// Preserve network classification without retaining provider text or addresses.
	errEmailPushTransient = &net.OpError{Op: "email push reconciliation", Err: errEmailPushIncomplete}
)

const emailPushProviderSyncFailed = "provider synchronization failed"

// Retain only fixed stage labels and allowlisted remediation. A provider cause
// may contain credential or URL data and must not enter receipts or status.
type emailPushReconciliationError struct {
	sourceID  int64
	reason    string
	transient bool
}

func (e *emailPushReconciliationError) Error() string {
	return fmt.Sprintf("%s: source %d: %s", errEmailPushIncomplete, e.sourceID, e.reason)
}

func (e *emailPushReconciliationError) Unwrap() error {
	if e.transient {
		return errEmailPushTransient
	}
	return errEmailPushIncomplete
}

func emailPushFailure(sourceID int64, reason string, cause error) error {
	if cause != nil {
		if previous, ok := errors.AsType[*emailPushReconciliationError](cause); ok {
			if previous != nil {
				reason = previous.reason
			}
		} else if credential, ok := errors.AsType[*provideridentity.GmailCredentialError](cause); ok {
			if credential != nil && credential.Remediation() != "" {
				reason += ": " + credential.Remediation()
			}
		} else if errors.Is(cause, context.Canceled) {
			reason += ": canceled"
		} else if errors.Is(cause, context.DeadlineExceeded) {
			reason += ": deadline exceeded"
		} else if _, ok := errors.AsType[net.Error](cause); ok {
			reason += ": transient network failure"
		}
	}
	return &emailPushReconciliationError{sourceID: sourceID, reason: reason, transient: syncerr.IsTransientNetwork(cause)}
}

// A binding is a startup snapshot of identity, never an alias/display selector.
// Re-read it at admission, inside the gate, after reconciliation and (for a
// broker adapter) before ACK. A replacement row cannot inherit pending hints.
type emailPushBinding struct {
	sourceID   int64
	sourceType string
	identifier string
	token      string
}

func (b emailPushBinding) current(ctx context.Context, st *store.Store) (*store.Source, error) {
	src, err := st.EmailPushBoundSource(ctx, b.sourceID, b.token)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrSourceNotFound) || errors.Is(err, store.ErrSourceRetired) {
			return nil, errEmailPushBinding
		}
		return nil, fmt.Errorf("read email push source: %w", err)
	}
	if src.SourceType != b.sourceType || src.Identifier != b.identifier || src.HistoryOnly || src.MergedIntoSourceID != 0 {
		return nil, errEmailPushBinding
	}
	return src, nil
}

func emailPushBindings(ctx context.Context, cfg *config.Config, st *store.Store, sched *scheduler.Scheduler) (map[string]emailPushBinding, error) {
	if err := cfg.EmailPush.Validate(); err != nil {
		return nil, err
	}
	bindings := make(map[string]emailPushBinding, len(cfg.EmailPush.IMAPSourceIDs))
	for _, id := range cfg.EmailPush.IMAPSourceIDs {
		src, token, err := st.BindEmailPushSource(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("email push source %d: %w", id, errors.Join(errEmailPushBinding, err))
		}
		if src.SourceType != sourceTypeIMAP {
			return nil, fmt.Errorf("email push source %d is not IMAP: %w", id, errEmailPushBinding)
		}
		if src.HistoryOnly || src.MergedIntoSourceID != 0 {
			return nil, fmt.Errorf("email push source %d is retired: %w", id, errEmailPushBinding)
		}
		matching := 0
		for _, account := range cfg.Accounts {
			if account.Email == src.Identifier {
				if !account.Enabled || account.Schedule == "" {
					return nil, fmt.Errorf("email push source %d needs an enabled account schedule: %w", id, errEmailPushBinding)
				}
				matching++
			}
		}
		if matching != 1 || !sched.IsScheduled(src.Identifier) {
			return nil, fmt.Errorf("email push source %d needs exactly one registered canonical account schedule: %w", id, errEmailPushBinding)
		}
		if _, duplicate := bindings[src.Identifier]; duplicate {
			return nil, fmt.Errorf("email push source %d shares a canonical account key: %w", id, errEmailPushBinding)
		}
		bindings[src.Identifier] = emailPushBinding{sourceID: id, sourceType: src.SourceType, identifier: src.Identifier, token: token}
	}
	return bindings, nil
}

type emailPushSourceRun func(context.Context, *store.Source) (*gmail.SyncSummary, error)

// emailPushWakeSync is installed only for provider wake passes. Ordinary
// cron/manual selectors and their status/error conventions remain unchanged.
func emailPushWakeSync(bindings map[string]emailPushBinding, st *store.Store, run emailPushSourceRun) scheduler.SyncFunc {
	return func(ctx context.Context, identifier string) error {
		binding, ok := bindings[identifier]
		if !ok {
			return errEmailPushBinding
		}
		src, err := binding.current(ctx, st)
		if err != nil {
			return err
		}
		summary, err := run(ctx, src)
		if err != nil {
			return emailPushFailure(binding.sourceID, emailPushProviderSyncFailed, err)
		}
		if jobctx.StopRequested(ctx) {
			return emailPushFailure(binding.sourceID, "provider synchronization stopped", nil)
		}
		if summary == nil {
			return emailPushFailure(binding.sourceID, "provider returned no completion summary", nil)
		}
		if summary.Errors != 0 {
			return emailPushFailure(binding.sourceID, "provider reported acquisition errors", nil)
		}
		if _, err := binding.current(ctx, st); err != nil {
			return err
		}
		// Nil provider errors do not establish that all acquisition/recovery
		// work completed. Preserve debt and cursors, but fail the receipt.
		items, err := st.ListUnresolvedSyncItemErrors(ctx, binding.sourceID)
		if err != nil {
			return emailPushFailure(binding.sourceID, "reading acquisition debt failed", err)
		}
		if len(items) != 0 {
			return emailPushFailure(binding.sourceID, "unresolved acquisition errors remain", nil)
		}
		checkpoint, err := st.GetLatestCheckpointedSyncContext(ctx, binding.sourceID)
		if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
			return emailPushFailure(binding.sourceID, "reading recovery checkpoint failed", err)
		}
		if checkpoint != nil {
			return emailPushFailure(binding.sourceID, "recovery checkpoint remains", nil)
		}
		return nil
	}
}

// runEmailPushSource reuses the existing ingestion machinery with an exact row.
// In particular, the Gmail path never calls a selector dispatcher or creates a
// source when one has disappeared. Its stricter result is local to wake passes.
func runEmailPushSource(ctx context.Context, src *store.Source, st *store.Store, state *invocation, getOAuthMgr func(context.Context, string) (*oauth.Manager, error)) (*gmail.SyncSummary, error) {
	ctx = withInvocation(ctx, state)
	if src.SourceType == sourceTypeIMAP {
		return runScheduledIMAPSync(ctx, src, st, state)
	}
	if src.SourceType != sourceTypeGmail {
		return nil, errEmailPushBinding
	}
	client, _, err := newDaemonGmailClient(ctx, src.Identifier, src, getOAuthMgr, state, oauth.Scopes)
	if err != nil {
		return nil, emailPushFailure(src.ID, "initializing Gmail client failed", err)
	}
	defer func() { _ = client.Close() }()
	opts := messagesync.DefaultOptions()
	opts.AttachmentsDir = state.cfg.AttachmentsDir()
	syncer := newMessageSyncer(client, st, opts, state).WithLogger(state.logger)
	return syncer.IncrementalWithHistoryRecovery(ctx, src, nil)
}

type emailInboxWatcher interface {
	WatchInbox(ctx context.Context, wake func()) error
	Close() error
}

func newEmailPushIMAPWatcher(ctx context.Context, src *store.Source, state *invocation) (emailInboxWatcher, error) {
	if src.SourceType != sourceTypeIMAP {
		return nil, errEmailPushBinding
	}
	apiClient, err := buildAPIClient(withInvocation(ctx, state), src, nil, nil)
	if err != nil {
		return nil, imaplib.ErrIdleAuthentication
	}
	watcher, ok := apiClient.(emailInboxWatcher)
	if !ok {
		_ = apiClient.Close()
		return nil, imaplib.ErrIdleAuthentication
	}
	return watcher, nil
}

type emailPushRuntime struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (r *emailPushRuntime) Stop() {
	if r == nil {
		return
	}
	r.cancel()
	r.wg.Wait()
}

func startEmailPush(ctx context.Context, cfg *config.Config, st *store.Store, sched *scheduler.Scheduler, state *invocation, getOAuthMgr func(context.Context, string) (*oauth.Manager, error), maintenance *attachmentMaintenance) (*emailPushRuntime, error) {
	bindings, err := emailPushBindings(ctx, cfg, st, sched)
	if err != nil {
		return nil, err
	}
	listenerCtx, cancel := context.WithCancel(ctx)
	runtime := &emailPushRuntime{cancel: cancel}
	if len(bindings) == 0 {
		return runtime, nil
	}
	run := func(ctx context.Context, src *store.Source) (*gmail.SyncSummary, error) {
		ctx = withInvocation(ctx, state)
		var summary *gmail.SyncSummary
		err := runScheduledSource(ctx, maintenance, true, func(ctx context.Context) error {
			var err error
			summary, err = runEmailPushSource(ctx, src, st, state, getOAuthMgr)
			return err
		})
		if !jobctx.StopRequested(ctx) {
			err = errors.Join(err, rebuildCacheAfterScheduledSourceRun(context.WithoutCancel(ctx), src.Identifier))
		}
		return summary, err
	}
	sched.WithAccountWakeSyncFunc(emailPushWakeSync(bindings, st, run))
	for _, binding := range bindings {
		runtime.wg.Go(func() {
			runIMAPPushListener(listenerCtx, binding, st, sched, state)
		})
	}
	return runtime, nil
}

func runIMAPPushListener(ctx context.Context, binding emailPushBinding, st *store.Store, sched *scheduler.Scheduler, state *invocation) {
	logger := state.logger
	listenerCtx, stop := context.WithCancel(ctx)
	defer stop()
	retry := func(attempt int) bool {
		delay := imapPushRetryDelay(attempt)
		logger.Warn("email push IMAP reconnecting; polling fallback retained", "source_id", binding.sourceID, "retry_in", delay)
		return waitEmailPushRetry(listenerCtx, delay)
	}
	for attempt := 0; listenerCtx.Err() == nil; attempt++ {
		src, err := binding.current(listenerCtx, st)
		if err != nil {
			if errors.Is(err, errEmailPushBinding) {
				logger.Info("email push source unavailable; polling fallback retained", "source_id", binding.sourceID)
				return
			}
			if !retry(attempt) {
				return
			}
			continue
		}
		watcher, err := newEmailPushIMAPWatcher(listenerCtx, src, state)
		if err == nil {
			sessionCtx, cancelSession := context.WithCancelCause(listenerCtx)
			admit := func() {
				if _, err := binding.current(sessionCtx, st); err != nil {
					if errors.Is(err, errEmailPushBinding) {
						stop()
					}
					cancelSession(err)
					return
				}
				if _, err := sched.WakeAccount(binding.identifier); err != nil {
					stop()
				}
			}
			// No-IDLE servers still receive the startup catchup before polling
			// resumes. Established listening supplies another coalesced hint.
			admit()
			connected := false
			err = watcher.WatchInbox(sessionCtx, func() {
				if !connected {
					connected = true
					attempt = 0
					logger.Info("email push IMAP listener connected", "source_id", binding.sourceID)
				}
				admit()
			})
			// Capture before cleanup: only a failed source check should force
			// a reconnect, not cancelSession's ordinary cancellation below.
			cause := context.Cause(sessionCtx)
			cancelSession(nil)
			_ = watcher.Close()
			if cause != nil && listenerCtx.Err() == nil {
				err = imaplib.ErrIdleDisconnected
			}
		}
		if listenerCtx.Err() != nil {
			return
		}
		if !errors.Is(err, imaplib.ErrIdleDisconnected) {
			reason := "authentication or configuration failure"
			if errors.Is(err, imaplib.ErrIdleUnsupported) {
				reason = "IDLE unsupported"
			}
			logger.Warn("email push IMAP listener stopped; polling fallback retained", "source_id", binding.sourceID, "reason", reason)
			return
		}
		if !retry(attempt) {
			return
		}
	}
}

func imapPushRetryDelay(attempt int) time.Duration {
	base := time.Second * time.Duration(1<<min(attempt, 5))
	return base + time.Duration(rand.Int64N(int64(base))) //nolint:gosec // Retry jitter is not a security primitive.
}

func waitEmailPushRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
