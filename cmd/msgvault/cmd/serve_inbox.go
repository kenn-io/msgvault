package cmd

import (
	"context"
	"fmt"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/gmail"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
	"path/filepath"
)

var _ api.InboxController = (*storeAPIAdapter)(nil)

func prepareInboxRuntime(ctx context.Context, cfg *config.Config, st *store.Store) ([]byte, error) {
	if cfg == nil || cfg.HomeDir == "" || st == nil {
		return nil, inboxcontrol.ErrInternal
	}
	keyPath := filepath.Join(cfg.HomeDir, "inbox-preview.key")
	key, err := inboxcontrol.EnsurePreviewKey(keyPath)
	if err != nil {
		return nil, fmt.Errorf("prepare inbox preview key %q: %w", keyPath, err)
	}
	if err := st.RecoverInboxReceipts(ctx); err != nil {
		return nil, fmt.Errorf("recover inbox dispatch receipts: %w", err)
	}
	return key, nil
}

func (a *storeAPIAdapter) ControlInbox(ctx context.Context, request inboxcontrol.Request, principal inboxcontrol.Principal, authorize func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error, acquireWrite func(context.Context) (func(), error)) (*inboxcontrol.Result, error) {
	result, err := a.inboxControlService(a.checkedInboxAuthorize(authorize)).Control(a.invocationContext(ctx), request, principal, acquireWrite)
	if request.Operation == inboxcontrol.OpReconcile || (request.Operation.IsMutation() && !request.DryRun) {
		if result != nil && result.Receipt != nil && result.Receipt.Status == inboxcontrol.StatusVerified && result.After != nil {
			a.refreshInboxCache(ctx)
		}
	}
	return result, err
}

// checkedInboxAuthorize binds fresh actions to active archive identities and
// saved-result replay to the retained source, original identity and live
// receipt authority. The latter tolerates sync tombstones while still refusing
// deleted items and removed sources.
func (a *storeAPIAdapter) checkedInboxAuthorize(authorize func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error) func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error {
	return func(ctx context.Context, caller inboxcontrol.Principal, intent inboxcontrol.Request) error {
		if authorize == nil {
			return inboxcontrol.ErrDenied
		}
		if err := authorize(ctx, caller, intent); err != nil {
			return err
		}
		var receipt *inboxcontrol.Receipt
		sourceID := int64(0)
		if intent.Target != nil {
			sourceID = intent.Target.SourceID
		} else if intent.Source != nil {
			sourceID = intent.Source.SourceID
		}
		if intent.ReceiptID != "" {
			var err error
			receipt, err = a.store.GetInboxReceipt(ctx, intent.ReceiptID)
			if err != nil {
				return inboxcontrol.ErrInternal
			}
		} else if intent.IdempotencyKey != "" && sourceID > 0 {
			var err error
			receipt, err = a.store.LookupInboxReceipt(ctx, caller.ID, sourceID, intent.IdempotencyKey)
			if err != nil {
				return inboxcontrol.ErrInternal
			}
		}
		if receipt == nil {
			_, _, err := a.inboxBinding(ctx, intent)
			return err
		}
		if err := a.store.ValidateInboxReceiptAuthorityContext(ctx, receipt.ID); err != nil {
			return err
		}
		var source inboxcontrol.SourceIdentity
		if receipt.Intent.Target != nil {
			target := receipt.Intent.Target
			source = inboxcontrol.SourceIdentity{SourceID: target.SourceID, SourceType: target.SourceType, SourceIdentifier: target.SourceIdentifier, AccountID: target.AccountID}
			if target.Scope == inboxcontrol.ScopeChat {
				if err := a.store.ValidateInboxTargetContext(ctx, *target); err != nil {
					return inboxcontrol.ErrDenied
				}
			} else if err := a.store.ValidateInboxItemIdentityContext(ctx, *target); err != nil {
				return inboxcontrol.ErrDenied
			}
		} else if receipt.Intent.Source != nil {
			source = *receipt.Intent.Source
		} else {
			return inboxcontrol.ErrDenied
		}
		_, _, err := a.inboxBinding(ctx, inboxcontrol.Request{Operation: inboxcontrol.OpGetCapabilities, Source: &source, DryRun: true})
		return err
	}
}

func (a *storeAPIAdapter) refreshInboxCache(ctx context.Context) {
	if a.inboxCacheRefresh == nil {
		return
	}
	if err := a.inboxCacheRefresh(ctx, "inbox-control"); err != nil && a.logger != nil {
		a.logger.Warn("queue analytics cache refresh after Inbox reconciliation", "error", err)
	}
}

func (a *storeAPIAdapter) inboxControlService(authorize func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error) *inboxcontrol.Service {
	return &inboxcontrol.Service{
		Ledger: a.store, Key: a.inboxKey, Authorize: authorize,
		Resolve: a.resolveInboxProvider,
		AcquireSource: func(ctx context.Context, sourceID int64) (func(), error) {
			execution, err := a.store.AcquireInboxExecutionContext(ctx, sourceID)
			if err != nil {
				return nil, err
			}
			return func() {
				if err := execution.Release(); err != nil && a.logger != nil {
					a.logger.Error("release inbox source ownership", "error", err)
				}
			}, nil
		},
		ReconcileState: func(ctx context.Context, before, after inboxcontrol.State) error {
			return a.store.ReconcileInboxProviderState(ctx, before.Target, before, after)
		},
	}
}

func (a *storeAPIAdapter) InboxTriageMappings(ctx context.Context, source inboxcontrol.SourceIdentity) (map[string]string, int64, error) {
	return a.store.InboxTriageMappings(a.invocationContext(ctx), source)
}

func (a *storeAPIAdapter) UpdateInboxTriageMappings(ctx context.Context, source inboxcontrol.SourceIdentity, entries map[string]string, expectedRevision int64, principal inboxcontrol.Principal, authorize func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error, acquireWrite func(context.Context) (func(), error)) (int64, error) {
	return a.inboxControlService(authorize).UpdateTriageMappings(a.invocationContext(ctx), source, entries, expectedRevision, principal, acquireWrite)
}

// inboxBinding rechecks live archive authority even for cached receipt reads and replay.
func (a *storeAPIAdapter) inboxBinding(ctx context.Context, request inboxcontrol.Request) (*store.Source, inboxcontrol.SourceIdentity, error) {
	var binding inboxcontrol.SourceIdentity
	if request.Target != nil {
		t := request.Target
		binding = inboxcontrol.SourceIdentity{SourceID: t.SourceID, SourceType: t.SourceType, SourceIdentifier: t.SourceIdentifier, AccountID: t.AccountID}
	} else if request.Source != nil {
		binding = *request.Source
	} else {
		return nil, binding, inboxcontrol.ErrDenied
	}
	source, err := a.store.GetSourceByIDContext(ctx, binding.SourceID)
	if err != nil || source == nil {
		return nil, binding, inboxcontrol.ErrDenied
	}
	kind := source.SourceType
	if kind == "" {
		kind = "gmail"
	}
	accountID := source.Identifier
	if kind == sourceTypeIMAP {
		if !source.SyncConfig.Valid {
			return nil, binding, inboxcontrol.ErrDenied
		}
		cfg, err := imaplib.ConfigFromJSON(source.SyncConfig.String)
		if err != nil || cfg.Username == "" || cfg.Identifier() != source.Identifier {
			return nil, binding, inboxcontrol.ErrDenied
		}
		accountID = cfg.Username
	}
	if binding.SourceType != kind || binding.SourceIdentifier != source.Identifier || binding.AccountID != accountID {
		return nil, binding, inboxcontrol.ErrDenied
	}
	if request.Target != nil && binding.SourceType == "beeper" {
		if request.Target.Scope != inboxcontrol.ScopeChat {
			return nil, binding, inboxcontrol.ErrUnavailable
		}
		if err := a.store.ValidateInboxTargetContext(ctx, *request.Target); err != nil {
			return nil, binding, inboxcontrol.ErrDenied
		}
		return source, binding, nil
	}
	if request.Target != nil {
		if request.Target.Scope != inboxcontrol.ScopeMessage {
			return nil, binding, inboxcontrol.ErrUnavailable
		}
		if original, ok := inboxcontrol.ReceiptAuthorityTarget(ctx); ok && original == *request.Target {
			if err := a.store.ValidateInboxItemIdentityContext(ctx, original); err != nil {
				return nil, binding, inboxcontrol.ErrDenied
			}
			return source, binding, nil
		}
		// Only Service recovery can supply this hint, after validating a durable
		// mapping against the original receipt and signed projected destination.
		if mapped, ok := inboxcontrol.ReceiptMoveTarget(ctx); ok && mapped == *request.Target {
			if err := a.store.ValidateInboxItemIdentityContext(ctx, mapped); err != nil {
				return nil, binding, inboxcontrol.ErrDenied
			}
			return source, binding, nil
		}
		native, err := a.store.EmailTagTargetContext(ctx, request.Target.ItemID, request.Target.Mailbox)
		if err != nil || native.SourceID != request.Target.SourceID || native.Provider != request.Target.SourceType || native.SourceMessageID != request.Target.ProviderID || native.Mailbox != request.Target.Mailbox || native.UIDValidity != request.Target.UIDValidity || native.UID != request.Target.UID {
			return nil, binding, inboxcontrol.ErrDenied
		}
	}
	return source, binding, nil
}
func (a *storeAPIAdapter) resolveInboxProvider(ctx context.Context, request inboxcontrol.Request) (inboxcontrol.Provider, error) {
	source, binding, err := a.inboxBinding(ctx, request)
	if err != nil {
		return nil, err
	}
	if binding.SourceType == sourceTypeIMAP {
		factory := a.emailTagClientFactory
		if factory == nil {
			factory = func(ctx context.Context, source *store.Source) (gmail.API, error) {
				return buildAPIClient(ctx, source, oauthManagerCache(invocationFromContext(ctx)), nil)
			}
		}
		client, err := factory(ctx, source)
		if err != nil {
			return nil, inboxcontrol.ErrUnavailable
		}
		native, ok := client.(*imaplib.Client)
		if !ok || native == nil {
			if client != nil {
				_ = client.Close()
			}
			return nil, inboxcontrol.ErrUnavailable
		}
		return imaplib.NewInboxProvider(native, binding), nil
	}
	if binding.SourceType == sourceTypeMSMail {
		return a.resolveMicrosoftInboxProvider(ctx, source, binding, request)
	}
	if binding.SourceType == "beeper" {
		return a.resolveBeeperInboxProvider(ctx, request, source, binding)
	}
	if binding.SourceType != "gmail" {
		return nil, inboxcontrol.ErrUnavailable
	}
	write := request.Operation.IsMutation()
	capability := a.gmailInboxWriteCapability(ctx, source)
	if write && capability != inboxcontrol.CapabilitySupported {
		if capability == inboxcontrol.CapabilityPermissionRequired {
			return nil, inboxcontrol.ErrDenied
		}
		return nil, inboxcontrol.ErrUnavailable
	}
	factory := a.emailTagClientFactory
	if factory == nil {
		factory = func(ctx context.Context, source *store.Source) (gmail.API, error) {
			scopes := oauth.ScopesGmailReadonly
			if write {
				scopes = []string{oauth.ScopeGmailModify}
			}
			state := invocationFromContext(ctx)
			client, _, err := newDaemonGmailClient(ctx, source.Identifier, source, oauthManagerCache(state), state, scopes)
			return client, err
		}
	}
	client, err := factory(ctx, source)
	if err != nil {
		return nil, inboxcontrol.ErrUnavailable
	}
	native, ok := client.(*gmail.Client)
	if !ok || native == nil {
		if client != nil {
			_ = client.Close()
		}
		return nil, inboxcontrol.ErrUnavailable
	}
	return gmail.NewInboxProvider(native, binding).WithWriteCapability(capability), nil
}
func (a *storeAPIAdapter) gmailInboxWriteCapability(ctx context.Context, source *store.Source) inboxcontrol.CapabilityStatus {
	if a.config != nil && a.config.OAuth.ServiceAccountKeyFor(sourceOAuthApp(source)) != "" {
		return inboxcontrol.CapabilitySupported
	}
	manager, err := oauthManagerCache(invocationFromContext(ctx))(ctx, sourceOAuthApp(source))
	if err != nil || !manager.HasScopeMetadata(ctx, source.Identifier) {
		return inboxcontrol.CapabilityUnavailable
	}
	if oauth.GrantCoversAnyScope(manager.GrantedScopes(ctx, source.Identifier), []string{oauth.ScopeGmailModify, oauth.ScopeGmailFull}) {
		return inboxcontrol.CapabilitySupported
	}
	return inboxcontrol.CapabilityPermissionRequired
}

// ResolveMessageTagTarget binds legacy Gmail tag consumers to the same exact
// archive identity used by signed Inbox requests. It makes no provider call.
func (a *storeAPIAdapter) ResolveMessageTagTarget(ctx context.Context, id int64, mailbox string) (inboxcontrol.Target, error) {
	native, err := a.store.EmailTagTargetContext(ctx, id, mailbox)
	if err != nil {
		return inboxcontrol.Target{}, err
	}
	source, err := a.store.GetSourceByIDContext(ctx, native.SourceID)
	if err != nil {
		return inboxcontrol.Target{}, emailtags.Failure("local_failed", "Cannot resolve tag source", nil, err)
	}
	target := inboxcontrol.Target{SourceID: source.ID, SourceType: native.Provider, SourceIdentifier: source.Identifier, AccountID: source.Identifier, Scope: inboxcontrol.ScopeMessage, ItemID: id, ProviderID: native.SourceMessageID, Mailbox: native.Mailbox, UIDValidity: native.UIDValidity, UID: native.UID}
	if err := target.Validate(); err != nil {
		return inboxcontrol.Target{}, emailtags.Failure("stale_identity", "Cannot resolve exact tag target", nil, err)
	}
	return target, nil
}

var _ api.InboxTriageController = (*storeAPIAdapter)(nil)

func (a *storeAPIAdapter) PreviewInboxTriage(ctx context.Context, input inboxcontrol.TriageInput, p inboxcontrol.Principal, authorize func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error) (*inboxcontrol.TriageProposal, error) {
	return a.inboxControlService(authorize).PreviewTriage(a.invocationContext(ctx), input, p)
}
func (a *storeAPIAdapter) ApplyInboxTriage(ctx context.Context, proposal inboxcontrol.TriageProposal, p inboxcontrol.Principal, authorize func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error, gate func(context.Context) (func(), error)) ([]inboxcontrol.Result, error) {
	results, err := a.inboxControlService(a.checkedInboxAuthorize(authorize)).ApplyTriage(a.invocationContext(ctx), proposal, p, gate)
	for _, result := range results {
		if result.Receipt != nil && result.Receipt.Status == inboxcontrol.StatusVerified && result.After != nil {
			a.refreshInboxCache(ctx)
			break
		}
	}
	return results, err
}
