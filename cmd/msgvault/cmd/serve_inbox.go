package cmd

import (
	"context"
	"fmt"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/gmail"
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
	checkedAuthorize := func(ctx context.Context, caller inboxcontrol.Principal, intent inboxcontrol.Request) error {
		if authorize == nil {
			return inboxcontrol.ErrDenied
		}
		if err := authorize(ctx, caller, intent); err != nil {
			return err
		}
		if request.ReceiptID != "" {
			if err := a.store.ValidateInboxReceiptAuthorityContext(ctx, request.ReceiptID); err != nil {
				return err
			}
		} else if request.IdempotencyKey != "" {
			sourceID := int64(0)
			if request.Target != nil {
				sourceID = request.Target.SourceID
			} else if request.Source != nil {
				sourceID = request.Source.SourceID
			}
			receipt, err := a.store.LookupInboxReceipt(ctx, caller.ID, sourceID, request.IdempotencyKey)
			if err != nil {
				return inboxcontrol.ErrInternal
			}
			if receipt != nil {
				if err := a.store.ValidateInboxReceiptAuthorityContext(ctx, receipt.ID); err != nil {
					return err
				}
			}
		}
		_, _, err := a.inboxBinding(ctx, intent)
		return err
	}
	controlCtx := a.invocationContext(ctx)
	service := a.inboxControlService(checkedAuthorize)
	var reconciledSourceID int64
	var reconciledSourceIdentifier string
	reconcileState := service.ReconcileState
	service.ReconcileState = func(ctx context.Context, before, after inboxcontrol.State) error {
		if err := reconcileState(ctx, before, after); err != nil {
			return err
		}
		if before.Target != (inboxcontrol.Target{}) {
			reconciledSourceID = before.Target.SourceID
			reconciledSourceIdentifier = before.Target.SourceIdentifier
		} else {
			reconciledSourceID = before.Source.SourceID
			reconciledSourceIdentifier = before.Source.SourceIdentifier
		}
		return nil
	}
	result, err := service.Control(controlCtx, request, principal, acquireWrite)
	if reconciledSourceIdentifier != "" {
		a.refreshInboxAnalyticsCache(controlCtx, reconciledSourceID, reconciledSourceIdentifier)
	}
	return result, err
}

func (a *storeAPIAdapter) refreshInboxAnalyticsCache(ctx context.Context, sourceID int64, identifier string) {
	if a.draftCacheRefresh == nil {
		return
	}
	refreshCtx, cancel := localDraftEvidenceContext(ctx)
	defer cancel()
	if err := a.draftCacheRefresh(refreshCtx, identifier); err != nil && a.logger != nil {
		a.logger.Error("Inbox analytics cache refresh failed", "source_id", sourceID, "error", err)
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
	if binding.SourceType != kind || binding.SourceIdentifier != source.Identifier || binding.AccountID != source.Identifier {
		return nil, binding, inboxcontrol.ErrDenied
	}
	if request.Target != nil {
		if request.Target.Scope != inboxcontrol.ScopeMessage {
			return nil, binding, inboxcontrol.ErrUnavailable
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
