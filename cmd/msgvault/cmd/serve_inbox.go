package cmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gmail"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/msmail"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
)

var _ api.InboxController = (*storeAPIAdapter)(nil)

func prepareInboxRuntime(ctx context.Context, cfg *config.Config, st *store.Store) ([]byte, error) {
	if cfg == nil || cfg.HomeDir == "" || st == nil {
		return nil, inboxcontrol.ErrInternal
	}
	key, err := inboxcontrol.EnsurePreviewKey(filepath.Join(cfg.HomeDir, "inbox-preview.key"))
	if err != nil {
		return nil, fmt.Errorf("prepare inbox preview key: %w", err)
	}
	if err := st.RecoverInboxReceipts(ctx); err != nil {
		return nil, fmt.Errorf("recover inbox dispatch receipts: %w", err)
	}
	return key, nil
}

func (a *storeAPIAdapter) ControlInbox(ctx context.Context, request inboxcontrol.Request, principal inboxcontrol.Principal, authorize func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error, acquireWrite func(context.Context) (func(), error)) (*inboxcontrol.Result, error) {
	return a.inboxControlService(authorize).Control(a.invocationContext(ctx), request, principal, acquireWrite)
}

func (a *storeAPIAdapter) inboxControlService(authorize func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error) *inboxcontrol.Service {
	return &inboxcontrol.Service{
		Ledger: a.store, Key: a.inboxKey, Authorize: authorize,
		Resolve: a.resolveInboxProvider,
		AcquireSource: func(ctx context.Context, sourceID int64) (func(), error) {
			execution, err := a.store.AcquireSyncExecutionContext(ctx, sourceID)
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

func (a *storeAPIAdapter) resolveInboxProvider(ctx context.Context, request inboxcontrol.Request) (inboxcontrol.Provider, error) {
	var binding inboxcontrol.SourceIdentity
	if request.Target != nil {
		target := request.Target
		binding = inboxcontrol.SourceIdentity{SourceID: target.SourceID, SourceType: target.SourceType, SourceIdentifier: target.SourceIdentifier, AccountID: target.AccountID}
	} else if request.Source != nil {
		binding = *request.Source
	} else {
		return nil, inboxcontrol.ErrInvalid
	}
	source, err := a.store.GetSourceByIDContext(ctx, binding.SourceID)
	if err != nil || source == nil {
		return nil, inboxcontrol.ErrDenied
	}
	if source.SourceType == "" {
		source.SourceType = "gmail"
	}
	account := source.Identifier
	if source.SourceType == "imap" {
		if !source.SyncConfig.Valid {
			return nil, inboxcontrol.ErrUnavailable
		}
		cfg, err := imaplib.ConfigFromJSON(source.SyncConfig.String)
		if err != nil || cfg.Identifier() != source.Identifier {
			return nil, inboxcontrol.ErrUnavailable
		}
		account = cfg.Username
	}
	if binding.SourceType != source.SourceType || binding.SourceIdentifier != source.Identifier || binding.AccountID != account {
		return nil, inboxcontrol.ErrDenied
	}
	if request.Target != nil {
		target := *request.Target
		mapped, hasMapping := inboxcontrol.ReceiptMoveTarget(ctx)
		if target.SourceType == "imap" && hasMapping && mapped == target {
			if err := a.store.ValidateInboxItemIdentityContext(ctx, target); err != nil {
				return nil, inboxcontrol.ErrDenied
			}
		} else if target.Scope == inboxcontrol.ScopeMessage {
			archived, err := a.store.EmailTagTargetContext(ctx, target.ItemID, target.Mailbox)
			if err != nil {
				return nil, inboxcontrol.ErrDenied
			}
			if archived.SourceID != target.SourceID || archived.Provider != target.SourceType || archived.SourceMessageID != target.ProviderID || archived.Mailbox != target.Mailbox || archived.UIDValidity != target.UIDValidity || archived.UID != target.UID {
				return nil, inboxcontrol.ErrDenied
			}
		} else if err := a.store.ValidateInboxTargetContext(ctx, target); err != nil {
			return nil, inboxcontrol.ErrDenied
		}
	}
	if a.inboxProviderFactory != nil {
		return a.inboxProviderFactory(ctx, source, request)
	}
	if source.SourceType == sourceTypeMSMail {
		write := request.Operation.IsMutation()
		if write && request.Operation != inboxcontrol.OpTags {
			return nil, inboxcontrol.ErrUnavailable
		}
		capability := a.microsoftInboxWriteCapability(ctx, source)
		if write && capability != inboxcontrol.CapabilitySupported {
			if capability == inboxcontrol.CapabilityPermissionRequired {
				return nil, inboxcontrol.ErrDenied
			}
			return nil, inboxcontrol.ErrUnavailable
		}
		if request.NativeTagCatalog {
			client, err := a.newMicrosoftTriageCatalogClient(ctx, source)
			if err != nil || client == nil {
				return nil, inboxcontrol.ErrUnavailable
			}
			return msmail.NewInboxProvider(client, binding).WithWriteCapability(capability), nil
		}
		client, err := a.newMicrosoftTagClient(ctx, source, write)
		if err != nil || client == nil {
			return nil, inboxcontrol.ErrUnavailable
		}
		return msmail.NewInboxProvider(client, binding).WithWriteCapability(capability), nil
	}
	if source.SourceType == "beeper" {
		if a.config == nil {
			return nil, inboxcontrol.ErrUnavailable
		}
		client, err := a.beeperDraftClient()
		if err != nil {
			return nil, inboxcontrol.ErrUnavailable
		}
		return beeper.NewInboxProvider(client, binding), nil
	}
	if source.SourceType == "imap" {
		factory := a.emailTagClientFactory
		if factory == nil {
			factory = func(ctx context.Context, source *store.Source) (gmail.API, error) {
				return buildAPIClient(ctx, source, oauthManagerCache(invocationFromContext(ctx)), oauth.ScopesGmailReadonly)
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
	if source.SourceType == "gmail" {
		write := request.Operation.IsMutation()
		writeCapability := a.gmailInboxWriteCapability(ctx, source)
		if write && writeCapability != inboxcontrol.CapabilitySupported {
			if writeCapability == inboxcontrol.CapabilityPermissionRequired {
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
		return gmail.NewInboxProvider(native, binding).WithWriteCapability(writeCapability), nil
	}
	return nil, inboxcontrol.ErrUnavailable
}

func (a *storeAPIAdapter) newMicrosoftTriageCatalogClient(ctx context.Context, source *store.Source) (*msmail.Client, error) {
	if a.microsoftTriageCatalogClientFactory != nil {
		return a.microsoftTriageCatalogClientFactory(ctx, source)
	}
	state := invocationFromContext(ctx)
	if state == nil || state.cfg == nil {
		return nil, errors.New("microsoft configuration is unavailable")
	}
	token, err := newGraphMailTriageManager(state).TokenSource(ctx, source.Identifier)
	if err != nil {
		return nil, err
	}
	return msmail.NewClient(msmail.GraphBaseURL, token, msmailQPS), nil
}

// Read and write scope probes inspect existing token metadata only. A missing
// or unreadable read grant is unknown; a verified read-only grant needs permission.
func (a *storeAPIAdapter) microsoftInboxWriteCapability(ctx context.Context, source *store.Source) inboxcontrol.CapabilityStatus {
	state := invocationFromContext(ctx)
	if state == nil || state.cfg == nil {
		return inboxcontrol.CapabilityUnavailable
	}
	read, err := newGraphMailManager(state).HasScopes(source.Identifier)
	if err != nil || !read {
		return inboxcontrol.CapabilityUnavailable
	}
	write, err := newGraphMailWriteManager(state).HasScopes(source.Identifier)
	if err != nil {
		return inboxcontrol.CapabilityUnavailable
	}
	if !write {
		return inboxcontrol.CapabilityPermissionRequired
	}
	return inboxcontrol.CapabilitySupported
}

// Missing saved scope metadata is unknown, rather than a modification grant.
// This probe never enrolls an account or expands its saved credentials.
func (a *storeAPIAdapter) gmailInboxWriteCapability(ctx context.Context, source *store.Source) inboxcontrol.CapabilityStatus {
	if a.config != nil && a.config.OAuth.ServiceAccountKeyFor(sourceOAuthApp(source)) != "" {
		return inboxcontrol.CapabilitySupported
	}
	manager, err := oauthManagerCache(invocationFromContext(ctx))(sourceOAuthApp(source))
	if err != nil || !manager.HasScopeMetadata(source.Identifier) {
		return inboxcontrol.CapabilityUnavailable
	}
	if oauth.GrantCoversAnyScope(manager.GrantedScopes(source.Identifier), []string{oauth.ScopeGmailModify, oauth.ScopeGmailFull}) {
		return inboxcontrol.CapabilitySupported
	}
	return inboxcontrol.CapabilityPermissionRequired
}

// InboxCandidates returns committed archive metadata within the invocation context.
func (a *storeAPIAdapter) InboxCandidates(ctx context.Context, source inboxcontrol.SourceIdentity, scope inboxcontrol.Scope, limit int, cursor string) (*inboxcontrol.CandidatePage, error) {
	return a.store.InboxCandidates(a.invocationContext(ctx), source, scope, limit, cursor)
}

// InboxContext reads bounded committed text within the invocation context.
func (a *storeAPIAdapter) InboxContext(ctx context.Context, request inboxcontrol.ContextRequest) (*inboxcontrol.Context, error) {
	return a.store.InboxContext(a.invocationContext(ctx), request)
}

var _ api.InboxTriageController = (*storeAPIAdapter)(nil)

func (a *storeAPIAdapter) PreviewInboxTriage(ctx context.Context, input inboxcontrol.TriageInput, p inboxcontrol.Principal, authorize func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error) (*inboxcontrol.TriageProposal, error) {
	return a.inboxControlService(authorize).PreviewTriage(a.invocationContext(ctx), input, p)
}
func (a *storeAPIAdapter) ApplyInboxTriage(ctx context.Context, proposal inboxcontrol.TriageProposal, p inboxcontrol.Principal, authorize func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error, gate func(context.Context) (func(), error)) ([]inboxcontrol.Result, error) {
	return a.inboxControlService(authorize).ApplyTriage(a.invocationContext(ctx), proposal, p, gate)
}
