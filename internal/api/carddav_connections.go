package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func (c *CardDAVController) root() *CardDAVController {
	if c.manager != nil {
		return c.manager
	}
	return c
}

func (c *CardDAVController) globalOperations() CardDAVOperations {
	if c.store == nil || c.cfg == nil {
		return c.Current()
	}
	return &cardDAVManagerOperations{Service: carddav.NewService(c.store, nil), controller: c.root()}
}

func (c *CardDAVController) configLock() *sync.RWMutex { return &c.root().mu }
func (c *CardDAVController) saveLock() *sync.Mutex     { return &c.root().saveMu }

func (c *CardDAVController) SetConnectionScheduleReconciler(reconcile func(string, config.CardDAVConfig, CardDAVOperations) error) {
	root := c.root()
	root.mu.Lock()
	defer root.mu.Unlock()
	root.reconcileConnectionSchedule = reconcile
}

func (c *CardDAVController) connection() string {
	if c.connectionName == "" {
		return config.DefaultCardDAVConnection
	}
	return c.connectionName
}

func (c *CardDAVController) configFrom(cfg *config.Config) config.CardDAVConfig {
	if c.connection() == config.DefaultCardDAVConnection {
		return cfg.CardDAV
	}
	return cfg.CardDAVConnections[c.connection()]
}

func (c *CardDAVController) bindingTokenDir() (string, error) {
	return carddav.ConnectionTokenDir(c.cfg.TokensDir(), c.connection())
}

// Select validates a public selector before registering a runtime. Setup may
// prepare a new name; reads and sync must refer to a saved connection.
func (c *CardDAVController) Select(name string, allowNew bool) (*CardDAVController, error) {
	if name == "" {
		name = config.DefaultCardDAVConnection
	}
	if err := config.ValidateCardDAVConnectionName(name); err != nil {
		return nil, fmt.Errorf("%w: invalid connection name", errCardDAVValidation)
	}
	root := c.root()
	if name == config.DefaultCardDAVConnection {
		return root, nil
	}
	root.ensureDependencies()
	root.mu.Lock()
	defer root.mu.Unlock()
	if root.cfg == nil {
		return nil, errCardDAVUnavailable
	}
	if _, exists := root.cfg.CardDAVConnections[name]; !exists && !allowNew {
		return nil, fmt.Errorf("%w: unknown connection", errCardDAVValidation)
	}
	if root.connections == nil {
		root.connections = make(map[string]*CardDAVController)
	}
	if child := root.connections[name]; child != nil {
		return child, nil
	}
	child := &CardDAVController{manager: root, connectionName: name, cfg: root.cfg, store: root.store,
		factory: root.factory, persistDiscovery: root.persistDiscovery,
		saveCredential: root.saveCredential, loadCredential: root.loadCredential, removeCredential: root.removeCredential}
	child.saveConfig = child.saveCardDAVConfig
	root.connections[name] = child
	return child, nil
}

func (c *CardDAVController) connectionNames() []string {
	root := c.root()
	root.mu.RLock()
	defer root.mu.RUnlock()
	names := make([]string, 0)
	if root.cfg == nil {
		return names
	}
	if root.cfg.CardDAV.BaseURL != "" || root.cfg.CardDAV.Username != "" {
		names = append(names, config.DefaultCardDAVConnection)
	}
	for name := range root.cfg.CardDAVConnections {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (c *CardDAVController) scopedCandidate(candidate cardDAVCandidate, generation int64) cardDAVCandidate {
	if service, ok := candidate.(*carddav.Service); ok {
		return service.ForConnection(c.connection(), generation)
	}
	return candidate
}

func (c *CardDAVController) scopedStoredBooks(ctx context.Context) ([]store.CardDAVAddressBook, error) {
	account, err := c.store.GetCardDAVAccountByNameContext(ctx, c.connection())
	if err != nil {
		return nil, err
	}
	if account == nil {
		return []store.CardDAVAddressBook{}, nil
	}
	return c.store.ListCardDAVAddressBooksContext(ctx, account.ID)
}

type CardDAVConnectionResponse struct {
	Connection string                `json:"connection"`
	Orphaned   bool                  `json:"orphaned"`
	AccountID  int64                 `json:"account_id,omitzero"`
	Provider   string                `json:"provider,omitempty"`
	OAuthApp   string                `json:"oauth_app,omitempty"`
	Status     CardDAVStatusResponse `json:"status"`
}

type CardDAVConnectionsResponse struct {
	Connections []CardDAVConnectionResponse `json:"connections"`
}

func (c *CardDAVController) Connections(ctx context.Context) (CardDAVConnectionsResponse, error) {
	result := CardDAVConnectionsResponse{Connections: make([]CardDAVConnectionResponse, 0)}
	if c == nil || c.cfg == nil || c.store == nil {
		return result, errCardDAVUnavailable
	}
	accounts, err := c.store.ListCardDAVAccountsContext(ctx)
	if err != nil {
		return result, errors.Join(errCardDAVStorage, err)
	}
	byName := make(map[string]store.CardDAVAccount, len(accounts))
	for _, account := range accounts {
		byName[account.ConnectionName] = account
	}
	for _, name := range c.connectionNames() {
		selected, err := c.Select(name, false)
		if err != nil {
			return result, err
		}
		status, err := selected.scopedStatus(ctx)
		if err != nil {
			return result, err
		}
		configured := selected.cardDAVConfigSnapshot()
		summary := CardDAVConnectionResponse{Connection: name, Provider: configured.Provider, OAuthApp: configured.OAuthApp, Status: status}
		if account, ok := byName[name]; ok {
			summary.AccountID = account.ID
		}
		result.Connections = append(result.Connections, summary)
		delete(byName, name)
	}
	for name, account := range byName {
		result.Connections = append(result.Connections, CardDAVConnectionResponse{
			Connection: name, AccountID: account.ID, Orphaned: true,
			Status: CardDAVStatusResponse{Account: &CardDAVStatusAccount{BaseURL: cardDAVStatusBaseURL(account.BaseURL), Username: account.Username}},
		})
	}
	slices.SortFunc(result.Connections, func(a, b CardDAVConnectionResponse) int {
		return strings.Compare(a.Connection, b.Connection)
	})
	return result, nil
}

func (c *CardDAVController) validateUniqueAccount(ctx context.Context, next config.CardDAVConfig) error {
	c.configLock().RLock()
	err := c.cfg.ValidateCardDAVConnectionIdentity(c.connection(), next)
	c.configLock().RUnlock()
	if err != nil {
		return err
	}
	accounts, err := c.store.ListCardDAVAccountsContext(ctx)
	if err != nil {
		return errors.Join(errCardDAVStorage, err)
	}
	for _, account := range accounts {
		if account.ConnectionName != c.connection() && next.SameAccount(config.CardDAVConfig{BaseURL: account.BaseURL, Username: account.Username}) {
			return fmt.Errorf("%w %q; restore that connection's configuration", config.ErrDuplicateCardDAVAccount, account.ConnectionName)
		}
	}
	return nil
}

func (c *CardDAVController) Status(ctx context.Context, name string) (CardDAVStatusResponse, error) {
	if name != "" {
		selected, err := c.Select(name, false)
		if err != nil {
			return CardDAVStatusResponse{}, err
		}
		return selected.scopedStatus(ctx)
	}
	if c == nil || c.manager != nil {
		return c.scopedStatus(ctx)
	}
	summaries, err := c.Connections(ctx)
	if err != nil {
		return CardDAVStatusResponse{}, err
	}
	// Retained accounts still contribute history after their config is removed.
	if len(summaries.Connections) == 0 || len(summaries.Connections) == 1 && summaries.Connections[0].Connection == config.DefaultCardDAVConnection {
		return c.scopedStatus(ctx)
	}
	var result CardDAVStatusResponse
	for _, summary := range summaries.Connections {
		status := summary.Status
		result.Configured = result.Configured || status.Configured
		result.Enabled = result.Enabled || status.Enabled
		result.Available = result.Available || status.Available && status.CredentialConfigured
		result.CredentialConfigured = result.CredentialConfigured || status.CredentialConfigured
	}
	runs, err := c.store.CardDAVSyncStatusContext(ctx, store.AllCardDAVAccounts)
	if err != nil {
		return result, errors.Join(errCardDAVStorage, err)
	}
	result.Active, result.Latest, result.LatestSuccessful = cardDAVRunResponse(runs.Active), cardDAVRunResponse(runs.Latest), cardDAVRunResponse(runs.LatestSuccessful)
	if err := c.attributeStatusRuns(ctx, &result); err != nil {
		return result, err
	}
	return result, nil
}

func (c *CardDAVController) storedAccountID(ctx context.Context) (int64, error) {
	if c.connection() == config.DefaultCardDAVConnection {
		return store.DefaultCardDAVAccountID, nil
	}
	account, err := c.store.GetCardDAVAccountByNameContext(ctx, c.connection())
	if err != nil || account == nil {
		return 0, err
	}
	return account.ID, nil
}

func (c *CardDAVController) reconcileGoogleSchedules(entry cardDAVGoogleAuthorization) error {
	root := c.root()
	root.saveMu.Lock()
	defer root.saveMu.Unlock()
	names := map[string]bool{entry.connection: true}
	if entry.connection == "" {
		names = map[string]bool{config.DefaultCardDAVConnection: true}
	}
	for _, name := range root.connectionNames() {
		selected, err := root.Select(name, false)
		if err != nil {
			return err
		}
		cfg := selected.cardDAVConfigSnapshot()
		if cfg.Provider == cardDAVProviderGoogle && cfg.Username == entry.email && cfg.OAuthApp == entry.oauthApp {
			names[name] = true
		}
	}
	var failures []error
	for name := range names {
		selected, err := root.Select(name, true)
		if err == nil {
			err = selected.reconcileCurrentSchedule()
		}
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func cardDAVConnectionParam() *huma.Param {
	return &huma.Param{Name: "connection", In: "query", Description: "Saved connection name; omit for all connections",
		Schema: &huma.Schema{Type: huma.TypeString, Pattern: "^[a-z][a-z0-9_-]{0,63}$"}}
}

func (c *CardDAVController) Sync(ctx context.Context, req CardDAVSyncRequest) (carddav.SyncResult, error) {
	if req.Connection != "" {
		selected, err := c.Select(req.Connection, false)
		if err != nil {
			return carddav.SyncResult{}, err
		}
		service := selected.Current()
		if service == nil {
			return carddav.SyncResult{}, carddav.ErrConnectionUnavailable
		}
		return service.Sync(ctx, carddav.SyncOptions{Full: req.Full, Trigger: store.CardDAVSyncTriggerManual})
	}
	var total carddav.SyncResult
	total.Status = "succeeded"
	total.Connections = make([]carddav.ConnectionSyncOutcome, 0)
	succeeded, failed := false, false
	for _, name := range c.connectionNames() {
		selected, err := c.Select(name, false)
		if err != nil {
			return total, err
		}
		if !selected.cardDAVConfigSnapshot().Enabled {
			continue
		}
		outcome := carddav.ConnectionSyncOutcome{Connection: name, Status: "failed"}
		account, err := c.store.GetCardDAVAccountByNameContext(ctx, name)
		if err == nil && account != nil {
			outcome.AccountID = account.ID
		}
		service := selected.Current()
		var result carddav.SyncResult
		if err == nil {
			if service == nil {
				err = carddav.ErrConnectionUnavailable
			} else {
				result, err = service.Sync(ctx, carddav.SyncOptions{Full: req.Full, Trigger: store.CardDAVSyncTriggerManual,
					OnRunStarted: func(id int64) { outcome.RunID = &id }})
			}
		}
		outcome.Books, outcome.Created, outcome.Updated, outcome.Removed = result.Books, result.Created, result.Updated, result.Removed
		total.Books += result.Books
		total.Created += result.Created
		total.Updated += result.Updated
		total.Removed += result.Removed
		if err == nil {
			outcome.Status = "succeeded"
			succeeded = true
		} else {
			failed = true
			outcome.ErrorCode, outcome.ErrorMessage = carddav.SyncFailure(err)
			if errors.Is(err, carddav.ErrConnectionUnavailable) {
				outcome.ErrorCode, outcome.ErrorMessage = "connection_unavailable", "CardDAV connection is unavailable. Repair its settings or credentials."
			}
			if errors.Is(err, store.ErrCardDAVSyncActive) {
				outcome.ErrorCode, outcome.ErrorMessage = "sync_active", "CardDAV synchronization is already running."
			}
			if result.Books > 0 {
				outcome.Status = "partial"
				succeeded = true
			}
		}
		total.Connections = append(total.Connections, outcome)
	}
	if len(total.Connections) == 0 {
		return carddav.SyncResult{}, carddav.ErrConnectionUnavailable
	}
	if failed {
		total.Status = "failed"
		if succeeded {
			total.Status = "partial"
		}
	}
	return total, nil
}

func (c *CardDAVController) accountNames(ctx context.Context) (map[int64]string, error) {
	accounts, err := c.store.ListCardDAVAccountsContext(ctx)
	if err != nil {
		return nil, errors.Join(errCardDAVStorage, err)
	}
	names := map[int64]string{store.DefaultCardDAVAccountID: config.DefaultCardDAVConnection}
	for _, account := range accounts {
		names[account.ID] = account.ConnectionName
	}
	return names, nil
}

func (c *CardDAVController) attributeStatusRuns(ctx context.Context, status *CardDAVStatusResponse) error {
	names, err := c.accountNames(ctx)
	if err != nil {
		return err
	}
	for _, run := range []*CardDAVRunResponse{status.Active, status.Latest, status.LatestSuccessful} {
		if run != nil {
			run.Connection = names[run.AccountID]
		}
	}
	return nil
}

func cardDAVQueryConnection(r *http.Request) (string, bool, error) {
	values, present := r.URL.Query()["connection"]
	if !present {
		return "", false, nil
	}
	if len(values) != 1 || values[0] == "" {
		return "", true, errCardDAVValidation
	}
	if err := config.ValidateCardDAVConnectionName(values[0]); err != nil {
		return "", true, errCardDAVValidation
	}
	return values[0], true, nil
}

func (s *Server) handleCardDAVConnections(w http.ResponseWriter, r *http.Request) {
	if s.cardDAV == nil {
		writeError(w, http.StatusServiceUnavailable, "carddav_unavailable", "CardDAV settings are unavailable")
		return
	}
	result, err := s.cardDAV.Connections(r.Context())
	if err != nil {
		s.writeCardDAVAccountError(w, err, "CardDAV connection lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, result)
}
