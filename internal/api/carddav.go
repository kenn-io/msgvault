package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

var (
	errCardDAVValidation = errors.New("invalid CardDAV request")
	errCardDAVUpstream   = errors.New("CardDAV upstream failure")
	errCardDAVStorage    = errors.New("CardDAV storage failure")
)

type CardDAVOperations interface {
	Sync(ctx context.Context, options carddav.SyncOptions) (carddav.SyncResult, error)
	ListBooks(ctx context.Context) ([]store.CardDAVAddressBook, error)
	SetBookRoles(ctx context.Context, bookID int64, roles carddav.BookRoles) error
	Publication(ctx context.Context, personID int64) (*store.CardDAVPublication, error)
	PublishPerson(ctx context.Context, personID int64) error
	UnpublishPerson(ctx context.Context, personID int64) error
	ListConflicts(ctx context.Context) ([]store.CardDAVConflict, error)
	GetConflict(ctx context.Context, conflictID int64) (*store.CardDAVConflict, error)
	ResolveConflict(ctx context.Context, conflictID int64, choice carddav.ResolutionChoice) error
}

type cardDAVCandidate interface {
	CardDAVOperations
	DiscoverConnection(ctx context.Context, baseURL string) (carddav.Discovery, error)
	PersistDiscovery(ctx context.Context, baseURL, username string, discovery carddav.Discovery, credentialsChanged bool) error
}

type cardDAVServiceFactory func(*store.Store, string, string, string) (cardDAVCandidate, error)

// CardDAVController owns the currently configured shared service and the
// discovery-first account setup transaction.
type CardDAVController struct {
	mu                 sync.RWMutex
	saveMu             sync.Mutex
	cfg                *config.Config
	store              *store.Store
	service            CardDAVOperations
	factory            cardDAVServiceFactory
	persistDiscovery   func(context.Context, cardDAVCandidate, string, string, carddav.Discovery, bool) error
	saveConfig         func(*config.CardDAVConfig, config.CardDAVConfig) (config.CardDAVConfig, error)
	saveCredential     func(string, carddav.Credential) error
	loadCredential     func(string) (carddav.Credential, error)
	saveLegacyPassword func(string, string) error
	loadLegacyPassword func(string) (string, error)
	removeCredential   func(string) error
	reconcileSchedule  func(config.CardDAVConfig, CardDAVOperations) error
}

// SetScheduleReconciler wires the daemon's live scheduler into successful
// account saves. The callback receives the newly persisted config and service.
func (c *CardDAVController) SetScheduleReconciler(reconcile func(config.CardDAVConfig, CardDAVOperations) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reconcileSchedule = reconcile
}

func NewCardDAVController(cfg *config.Config, st *store.Store) (*CardDAVController, error) {
	c := &CardDAVController{cfg: cfg, store: st, factory: newCardDAVService}
	c.saveCredential = carddav.SaveCredential
	c.loadCredential = carddav.LoadCredential
	c.saveLegacyPassword = carddav.SavePassword
	c.loadLegacyPassword = carddav.LoadLegacyPassword
	c.removeCredential = carddav.RemoveCredential
	c.persistDiscovery = func(ctx context.Context, service cardDAVCandidate, baseURL, username string, discovery carddav.Discovery, credentialsChanged bool) error {
		return service.PersistDiscovery(ctx, baseURL, username, discovery, credentialsChanged)
	}
	if cfg != nil {
		c.saveConfig = c.saveCardDAVConfig
	}
	configured := c.cardDAVConfigSnapshot()
	if cfg == nil || st == nil || strings.TrimSpace(configured.BaseURL) == "" {
		return c, nil
	}
	account, err := st.GetCardDAVAccountContext(context.Background())
	if err != nil {
		return nil, err
	}
	tokenDir := cfg.TokensDir()
	credential, err := c.loadCredential(tokenDir)
	if errors.Is(err, carddav.ErrCredentialNotBound) {
		legacyPassword, legacyErr := carddav.LoadLegacyPassword(tokenDir)
		if errors.Is(legacyErr, os.ErrNotExist) {
			return c, nil
		}
		if legacyErr != nil {
			return nil, legacyErr
		}
		if account == nil || account.ConnectionGeneration <= 0 ||
			configured.BaseURL != account.BaseURL || configured.Username != account.Username {
			return c, nil
		}
		credential = carddav.Credential{
			Password: legacyPassword, BaseURL: account.BaseURL, Username: account.Username,
			ConnectionGeneration: account.ConnectionGeneration,
		}
		if err := c.saveCredential(tokenDir, credential); err != nil {
			return nil, err
		}
	} else if errors.Is(err, os.ErrNotExist) {
		return c, nil
	} else if err != nil {
		return nil, err
	}
	if account == nil || credential.BaseURL != configured.BaseURL || credential.Username != configured.Username ||
		credential.BaseURL != account.BaseURL || credential.Username != account.Username ||
		credential.ConnectionGeneration != account.ConnectionGeneration {
		return c, nil
	}
	service, err := newCardDAVService(st, configured.BaseURL, configured.Username, credential.Password)
	if err != nil {
		return nil, err
	}
	c.service = service
	return c, nil
}

func newCardDAVService(st *store.Store, baseURL, username, password string) (cardDAVCandidate, error) {
	origin, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || origin.Scheme == "" || origin.Host == "" {
		return nil, errors.New("CardDAV base URL must be an absolute HTTP(S) URL")
	}
	client, err := carddav.NewClient(carddav.ClientOptions{CredentialOrigin: origin, Username: username, Password: password})
	if err != nil {
		return nil, err
	}
	return carddav.NewService(st, client), nil
}

func (c *CardDAVController) Current() CardDAVOperations {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.service
}

func (c *CardDAVController) cardDAVConfigSnapshot() config.CardDAVConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cfg == nil {
		return config.CardDAVConfig{}
	}
	return c.cfg.CardDAV
}

func (c *CardDAVController) publishCardDAVConfig(next config.CardDAVConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cfg != nil {
		c.cfg.CardDAV = next
	}
}

func (c *CardDAVController) ensureDependencies() {
	if c.factory == nil {
		c.factory = newCardDAVService
	}
	if c.persistDiscovery == nil {
		c.persistDiscovery = func(ctx context.Context, service cardDAVCandidate, baseURL, username string, discovery carddav.Discovery, credentialsChanged bool) error {
			return service.PersistDiscovery(ctx, baseURL, username, discovery, credentialsChanged)
		}
	}
	if c.cfg != nil && c.saveConfig == nil {
		c.saveConfig = c.saveCardDAVConfig
	}
	if c.saveCredential == nil {
		c.saveCredential = carddav.SaveCredential
	}
	if c.loadCredential == nil {
		c.loadCredential = carddav.LoadCredential
	}
	if c.saveLegacyPassword == nil {
		c.saveLegacyPassword = carddav.SavePassword
	}
	if c.loadLegacyPassword == nil {
		c.loadLegacyPassword = carddav.LoadLegacyPassword
	}
	if c.removeCredential == nil {
		c.removeCredential = carddav.RemoveCredential
	}
}

func (c *CardDAVController) saveCardDAVConfig(
	expected *config.CardDAVConfig, next config.CardDAVConfig,
) (config.CardDAVConfig, error) {
	path := c.cfg.ConfigFilePath()
	before, err := config.ReadConfigFile(path)
	if err != nil {
		return config.CardDAVConfig{}, err
	}
	latest, err := config.LoadConfigFile(before, "")
	if err != nil {
		return config.CardDAVConfig{}, err
	}
	previous := latest.CardDAV
	if expected != nil && previous != *expected {
		return previous, fmt.Errorf("%w: CardDAV settings changed", config.ErrConfigConflict)
	}
	after, err := config.EditConfigFile(path, before.ETag, []config.Edit{
		{Key: "carddav.base_url", Value: next.BaseURL},
		{Key: "carddav.username", Value: next.Username},
		{Key: "carddav.schedule", Value: next.Schedule},
		{Key: "carddav.enabled", Value: next.Enabled},
	})
	if err != nil {
		return previous, err
	}
	published, err := config.LoadConfigFile(after, "")
	if err != nil {
		return previous, err
	}
	c.publishCardDAVConfig(published.CardDAV)
	return previous, nil
}

func (c *CardDAVController) Test(ctx context.Context, req CardDAVAccountRequest) (CardDAVAccountResponse, error) {
	c.ensureDependencies()
	if err := validateCardDAVAccountRequest(req); err != nil {
		return CardDAVAccountResponse{}, err
	}
	password, err := c.passwordForRequest(ctx, req)
	if err != nil {
		return CardDAVAccountResponse{}, err
	}
	service, err := c.factory(c.store, req.BaseURL, req.Username, password)
	if err != nil {
		return CardDAVAccountResponse{}, errors.Join(errCardDAVValidation, err)
	}
	// Testing is deliberately non-persistent.
	discovery, err := service.DiscoverConnection(ctx, req.BaseURL)
	if err != nil {
		return CardDAVAccountResponse{}, errors.Join(errCardDAVUpstream, err)
	}
	return CardDAVAccountResponse{BaseURL: req.BaseURL, Username: req.Username, Enabled: *req.Enabled, Schedule: req.Schedule, Books: len(discovery.Books)}, nil
}

func (c *CardDAVController) Save(ctx context.Context, req CardDAVAccountRequest) (CardDAVAccountResponse, error) {
	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	c.ensureDependencies()
	if err := validateCardDAVAccountRequest(req); err != nil {
		return CardDAVAccountResponse{}, err
	}
	password, err := c.passwordForRequest(ctx, req)
	if err != nil {
		return CardDAVAccountResponse{}, err
	}
	account, err := c.store.GetCardDAVAccountContext(ctx)
	if err != nil {
		return CardDAVAccountResponse{}, errors.Join(errCardDAVStorage, err)
	}
	tokenDir := c.cfg.TokensDir()
	previousCredential, previousCredentialErr := c.loadCredential(tokenDir)
	hadPreviousCredential := previousCredentialErr == nil
	previousLegacyPassword := ""
	hadPreviousLegacyCredential := false
	if req.Password != "" && errors.Is(previousCredentialErr, carddav.ErrCredentialNotBound) {
		previousLegacyPassword, err = c.loadLegacyPassword(tokenDir)
		if err != nil {
			return CardDAVAccountResponse{}, errors.Join(errCardDAVStorage, err)
		}
		hadPreviousLegacyCredential = true
	} else if previousCredentialErr != nil && !errors.Is(previousCredentialErr, os.ErrNotExist) {
		return CardDAVAccountResponse{}, errors.Join(errCardDAVStorage, previousCredentialErr)
	}
	credentialsChanged := !hadPreviousCredential || previousCredential.Password != password ||
		previousCredential.BaseURL != req.BaseURL || previousCredential.Username != req.Username
	if err := c.store.ValidateCardDAVConnectionChangeContext(
		ctx, req.BaseURL, req.Username, credentialsChanged,
	); err != nil {
		return CardDAVAccountResponse{}, errors.Join(errCardDAVStorage, err)
	}
	next := config.CardDAVConfig{
		BaseURL: req.BaseURL, Username: req.Username, Enabled: *req.Enabled, Schedule: req.Schedule,
	}
	connectionUnchanged := account != nil && account.BaseURL == req.BaseURL &&
		account.Username == req.Username && !credentialsChanged && req.Password == ""
	if connectionUnchanged {
		books, err := c.store.ListCardDAVAddressBooksContext(ctx)
		if err != nil {
			return CardDAVAccountResponse{}, errors.Join(errCardDAVStorage, err)
		}
		c.mu.RLock()
		service := c.service
		reconcileSchedule := c.reconcileSchedule
		c.mu.RUnlock()
		if service == nil {
			return CardDAVAccountResponse{}, errors.Join(errCardDAVStorage,
				errors.New("CardDAV service is unavailable for saved account"))
		}
		previous, err := c.saveConfig(nil, next)
		if err != nil {
			var rollbackConfigErr error
			if errors.Is(err, config.ErrConfigChanged) {
				_, rollbackConfigErr = c.saveConfig(&next, previous)
			}
			return CardDAVAccountResponse{}, errors.Join(errCardDAVStorage, err, rollbackConfigErr)
		}
		if reconcileSchedule != nil {
			if err := reconcileSchedule(next, service); err != nil {
				return CardDAVAccountResponse{}, errors.Join(errCardDAVStorage,
					fmt.Errorf("reconcile CardDAV schedule: %w", err))
			}
		}
		return CardDAVAccountResponse{
			BaseURL: req.BaseURL, Username: req.Username, Enabled: *req.Enabled,
			Schedule: req.Schedule, Books: len(books),
		}, nil
	}
	service, err := c.factory(c.store, req.BaseURL, req.Username, password)
	if err != nil {
		return CardDAVAccountResponse{}, errors.Join(errCardDAVValidation, err)
	}
	discovery, err := service.DiscoverConnection(ctx, req.BaseURL)
	if err != nil {
		return CardDAVAccountResponse{}, errors.Join(errCardDAVUpstream, err)
	}
	generation := int64(1)
	if account != nil {
		generation = account.ConnectionGeneration
		if account.BaseURL != req.BaseURL || account.Username != req.Username || credentialsChanged {
			generation++
		}
	}
	rollbackCredential := func() error {
		if hadPreviousCredential {
			return c.saveCredential(tokenDir, previousCredential)
		}
		if hadPreviousLegacyCredential {
			return c.saveLegacyPassword(tokenDir, previousLegacyPassword)
		}
		return c.removeCredential(tokenDir)
	}
	if err := c.saveCredential(tokenDir, carddav.Credential{
		Password: password, BaseURL: req.BaseURL, Username: req.Username, ConnectionGeneration: generation,
	}); err != nil {
		return CardDAVAccountResponse{}, errors.Join(errCardDAVStorage, err)
	}
	previous, err := c.saveConfig(nil, next)
	if err != nil {
		var rollbackConfigErr error
		if errors.Is(err, config.ErrConfigChanged) {
			_, rollbackConfigErr = c.saveConfig(&next, previous)
		}
		return CardDAVAccountResponse{}, errors.Join(errCardDAVStorage, err, rollbackConfigErr, rollbackCredential())
	}
	if err := c.persistDiscovery(ctx, service, req.BaseURL, req.Username, discovery, credentialsChanged); err != nil {
		_, rollbackConfigErr := c.saveConfig(&next, previous)
		return CardDAVAccountResponse{}, errors.Join(errCardDAVStorage, err, rollbackConfigErr, rollbackCredential())
	}
	c.mu.Lock()
	c.service = service
	reconcileSchedule := c.reconcileSchedule
	c.mu.Unlock()
	if reconcileSchedule != nil {
		if err := reconcileSchedule(next, service); err != nil {
			return CardDAVAccountResponse{}, errors.Join(errCardDAVStorage, fmt.Errorf("reconcile CardDAV schedule: %w", err))
		}
	}
	return CardDAVAccountResponse{BaseURL: req.BaseURL, Username: req.Username, Enabled: *req.Enabled, Schedule: req.Schedule, Books: len(discovery.Books)}, nil
}

func (c *CardDAVController) passwordForRequest(ctx context.Context, req CardDAVAccountRequest) (string, error) {
	if req.Password != "" {
		return req.Password, nil
	}
	credential, err := c.reusableCredential(ctx, req.BaseURL, req.Username)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: CardDAV password is required for a new connection", errCardDAVValidation)
		}
		if errors.Is(err, carddav.ErrCredentialNotBound) {
			return "", fmt.Errorf("%w: CardDAV password is required because the saved connection identity does not match", errCardDAVValidation)
		}
		return "", errors.Join(errCardDAVStorage, err)
	}
	return credential.Password, nil
}

func (c *CardDAVController) reusableCredential(
	ctx context.Context, baseURL, username string,
) (carddav.Credential, error) {
	if c == nil {
		return carddav.Credential{}, carddav.ErrCredentialNotBound
	}
	configured := c.cardDAVConfigSnapshot()
	if c.cfg == nil || c.store == nil || configured.BaseURL != baseURL || configured.Username != username {
		return carddav.Credential{}, carddav.ErrCredentialNotBound
	}
	loadCredential := c.loadCredential
	if loadCredential == nil {
		loadCredential = carddav.LoadCredential
	}
	credential, err := loadCredential(c.cfg.TokensDir())
	if err != nil {
		return carddav.Credential{}, err
	}
	account, err := c.store.GetCardDAVAccountContext(ctx)
	if err != nil {
		return carddav.Credential{}, err
	}
	if account == nil || credential.BaseURL != baseURL || credential.Username != username ||
		account.BaseURL != baseURL || account.Username != username ||
		credential.ConnectionGeneration != account.ConnectionGeneration {
		return carddav.Credential{}, carddav.ErrCredentialNotBound
	}
	return credential, nil
}

func (c *CardDAVController) passwordConfigured(ctx context.Context, baseURL, username string) bool {
	_, err := c.reusableCredential(ctx, baseURL, username)
	return err == nil
}

func validateCardDAVAccountRequest(req CardDAVAccountRequest) error {
	if strings.TrimSpace(req.BaseURL) == "" || strings.TrimSpace(req.Username) == "" {
		return fmt.Errorf("%w: CardDAV base URL and username are required", errCardDAVValidation)
	}
	if req.Enabled == nil {
		return fmt.Errorf("%w: CardDAV enabled is required", errCardDAVValidation)
	}
	origin, err := url.Parse(strings.TrimSpace(req.BaseURL))
	if err != nil || origin.User != nil || origin.Host == "" || (origin.Scheme != "http" && origin.Scheme != "https") {
		return fmt.Errorf("%w: CardDAV base URL must be an absolute HTTP(S) URL", errCardDAVValidation)
	}
	if req.Schedule != "" {
		if err := scheduler.ValidateCronExpr(req.Schedule); err != nil {
			return errors.Join(errCardDAVValidation, fmt.Errorf("invalid CardDAV schedule: %w", err))
		}
	}
	return nil
}

type CardDAVAccountRequest struct {
	BaseURL  string `json:"base_url"`
	Username string `json:"username"`
	Password string `json:"password,omitempty" writeOnly:"true"`
	Schedule string `json:"schedule,omitempty"`
	Enabled  *bool  `json:"enabled" nullable:"false"`
}
type CardDAVAccountResponse struct {
	BaseURL  string `json:"base_url"`
	Username string `json:"username"`
	Schedule string `json:"schedule,omitempty"`
	Enabled  bool   `json:"enabled"`
	Books    int    `json:"books"`
}
type CardDAVBookResponse struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	URL                string `json:"url"`
	WriteTarget        bool   `json:"write_target"`
	Subscribed         bool   `json:"subscribed"`
	LookupSource       bool   `json:"lookup_source"`
	NeedsFullReconcile bool   `json:"needs_full_reconcile"`
}
type CardDAVBooksResponse struct {
	Books []CardDAVBookResponse `json:"books"`
}
type CardDAVBookRolesRequest struct {
	WriteTarget  *bool `json:"write_target" nullable:"false"`
	Subscribed   *bool `json:"subscribed" nullable:"false"`
	LookupSource *bool `json:"lookup_source" nullable:"false"`
}
type CardDAVPublicationResponse struct {
	PersonID         int64  `json:"person_id"`
	Desired          bool   `json:"desired"`
	PendingOperation string `json:"pending_operation,omitempty"`
	Href             string `json:"href,omitempty"`
}
type CardDAVConflictResponse struct {
	ID              int64  `json:"id"`
	AddressBookID   int64  `json:"address_book_id"`
	Href            string `json:"href"`
	LocalTombstone  bool   `json:"local_tombstone"`
	RemoteTombstone bool   `json:"remote_tombstone"`
	Status          string `json:"status"`
}
type CardDAVConflictDetailResponse struct {
	ID              int64  `json:"id"`
	AddressBookID   int64  `json:"address_book_id"`
	Href            string `json:"href"`
	LocalVCard      string `json:"local_vcard,omitempty"`
	RemoteVCard     string `json:"remote_vcard,omitempty"`
	LocalTombstone  bool   `json:"local_tombstone"`
	RemoteTombstone bool   `json:"remote_tombstone"`
	Status          string `json:"status"`
}
type CardDAVConflictResolutionResponse struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
}
type CardDAVConflictsResponse struct {
	Conflicts []CardDAVConflictResponse `json:"conflicts"`
}
type CardDAVResolveRequest struct {
	Choice carddav.ResolutionChoice `json:"choice" enum:"keep_local,keep_remote"`
}
type CardDAVSyncRequest struct {
	Full bool `json:"full,omitempty"`
}

func (s *Server) registerCardDAVRoutes(api huma.API) {
	registerCardDAVJSONRouteWithRequest[CardDAVAccountRequest, CardDAVAccountResponse](api, "testCardDAVAccount", http.MethodPost, "/carddav/account/test", "Test a CardDAV account", s.handleCardDAVAccountTest, http.StatusBadRequest, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable)
	registerCardDAVJSONRouteWithRequest[CardDAVAccountRequest, CardDAVAccountResponse](api, "saveCardDAVAccount", http.MethodPut, "/carddav/account", "Discover and save a CardDAV account", s.handleCardDAVAccountSave, http.StatusBadRequest, http.StatusConflict, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable)
	registerCardDAVJSONRoute[CardDAVBooksResponse](api, "listCardDAVBooks", http.MethodGet, "/carddav/books", "List CardDAV address books", s.handleCardDAVBooks, http.StatusInternalServerError, http.StatusServiceUnavailable)
	registerCardDAVIDJSONRouteWithRequest[CardDAVBookRolesRequest, CardDAVBookResponse](api, "updateCardDAVBookRoles", http.MethodPatch, "/carddav/books/{id}", "id", "Update CardDAV address book roles", s.handleCardDAVBookRoles, http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusInternalServerError, http.StatusServiceUnavailable)
	registerCardDAVIDJSONRoute[CardDAVPublicationResponse](api, "getCardDAVPublication", http.MethodGet, "/carddav/publications/{person_id}", "person_id", "Get CardDAV publication state", s.handleCardDAVPublication, http.StatusBadRequest, http.StatusNotFound, http.StatusInternalServerError, http.StatusServiceUnavailable)
	registerCardDAVIDJSONRoute[CardDAVPublicationResponse](api, "publishCardDAVPerson", http.MethodPost, "/carddav/publications/{person_id}", "person_id", "Publish a person to CardDAV", s.handleCardDAVPublish, http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable)
	registerCardDAVIDJSONRoute[CardDAVPublicationResponse](api, "unpublishCardDAVPerson", http.MethodDelete, "/carddav/publications/{person_id}", "person_id", "Unpublish a person from CardDAV", s.handleCardDAVUnpublish, http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable)
	registerCardDAVJSONRoute[CardDAVConflictsResponse](api, "listCardDAVConflicts", http.MethodGet, "/carddav/conflicts", "List unresolved CardDAV conflicts", s.handleCardDAVConflicts, http.StatusInternalServerError, http.StatusServiceUnavailable)
	registerCardDAVIDJSONRoute[CardDAVConflictDetailResponse](api, "getCardDAVConflict", http.MethodGet, "/carddav/conflicts/{id}", "id", "Inspect a CardDAV conflict", s.handleCardDAVConflict, http.StatusBadRequest, http.StatusNotFound, http.StatusInternalServerError, http.StatusServiceUnavailable)
	registerCardDAVIDJSONRouteWithRequest[CardDAVResolveRequest, CardDAVConflictResolutionResponse](api, "resolveCardDAVConflict", http.MethodPost, "/carddav/conflicts/{id}/resolve", "id", "Resolve a CardDAV conflict", s.handleCardDAVResolve, http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable)
	registerCardDAVJSONRouteWithRequest[CardDAVSyncRequest, carddav.SyncResult](api, "syncCardDAV", http.MethodPost, "/carddav/sync", "Trigger CardDAV synchronization", s.handleCardDAVSync, http.StatusBadRequest, http.StatusConflict, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable)
}

func registerCardDAVJSONRoute[Resp any](api huma.API, operationID, method, path, summary string, handler http.HandlerFunc, errorStatuses ...int) {
	op := rawAPIV1Operation(operationID, method, path, summary)
	op.Responses = jsonResponsesFor[Resp](api)
	addErrorResponses(api, op.Responses, errorStatuses...)
	addCardDAVRetryAfterHeader(op.Responses)
	registerRawHumaRoute(api, op, handler)
}

func registerCardDAVJSONRouteWithRequest[Req, Resp any](api huma.API, operationID, method, path, summary string, handler http.HandlerFunc, errorStatuses ...int) {
	op := rawAPIV1Operation(operationID, method, path, summary)
	op.RequestBody = jsonRequestBodyFor[Req](api)
	op.Responses = jsonResponsesFor[Resp](api)
	addErrorResponses(api, op.Responses, errorStatuses...)
	addCardDAVRetryAfterHeader(op.Responses)
	registerRawHumaRoute(api, op, handler)
}

func cardDAVIDOperation(operationID, method, path, parameter, summary string) huma.Operation {
	op := rawAPIV1Operation(operationID, method, path, summary)
	minimum := float64(1)
	op.Parameters = append(op.Parameters, &huma.Param{Name: parameter, In: "path", Required: true,
		Schema: &huma.Schema{Type: huma.TypeInteger, Format: formatInt64, Minimum: &minimum}})
	return op
}

func registerCardDAVIDJSONRoute[Resp any](api huma.API, operationID, method, path, parameter, summary string, handler http.HandlerFunc, errorStatuses ...int) {
	op := cardDAVIDOperation(operationID, method, path, parameter, summary)
	op.Responses = jsonResponsesFor[Resp](api)
	addErrorResponses(api, op.Responses, errorStatuses...)
	addCardDAVRetryAfterHeader(op.Responses)
	registerRawHumaRoute(api, op, handler)
}

func registerCardDAVIDJSONRouteWithRequest[Req, Resp any](api huma.API, operationID, method, path, parameter, summary string, handler http.HandlerFunc, errorStatuses ...int) {
	op := cardDAVIDOperation(operationID, method, path, parameter, summary)
	op.RequestBody = jsonRequestBodyFor[Req](api)
	op.Responses = jsonResponsesFor[Resp](api)
	addErrorResponses(api, op.Responses, errorStatuses...)
	addCardDAVRetryAfterHeader(op.Responses)
	registerRawHumaRoute(api, op, handler)
}

func addCardDAVRetryAfterHeader(responses map[string]*huma.Response) {
	response := responses[httpStatusKey(http.StatusServiceUnavailable)]
	if response == nil {
		return
	}
	if response.Headers == nil {
		response.Headers = make(map[string]*huma.Param)
	}
	minimum := float64(0)
	response.Headers["Retry-After"] = &huma.Param{
		Description: "Seconds until CardDAV retry is safe",
		Schema: &huma.Schema{
			Type: huma.TypeInteger, Format: formatInt64, Minimum: &minimum,
		},
	}
}

func decodeCardDAV(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, 400, "bad_request", "Invalid JSON request")
		return false
	}
	return true
}
func (s *Server) cardDAVService(w http.ResponseWriter) CardDAVOperations {
	if s.cardDAV == nil {
		writeError(w, 503, "carddav_unavailable", "CardDAV is not configured")
		return nil
	}
	service := s.cardDAV.Current()
	if service == nil {
		writeError(w, 503, "carddav_unavailable", "CardDAV is not configured")
		return nil
	}
	return service
}
func (s *Server) handleCardDAVAccountTest(w http.ResponseWriter, r *http.Request) {
	if s.cardDAV == nil {
		writeError(w, http.StatusServiceUnavailable, "carddav_unavailable", "CardDAV setup is unavailable")
		return
	}
	var req CardDAVAccountRequest
	if !decodeCardDAV(w, r, &req) {
		return
	}
	result, err := s.cardDAV.Test(r.Context(), req)
	if err != nil {
		s.writeCardDAVAccountError(r.Context(), w, err, "CardDAV discovery failed")
		return
	}
	writeJSON(w, 200, result)
}
func (s *Server) handleCardDAVAccountSave(w http.ResponseWriter, r *http.Request) {
	if s.cardDAV == nil {
		writeError(w, http.StatusServiceUnavailable, "carddav_unavailable", "CardDAV setup is unavailable")
		return
	}
	var req CardDAVAccountRequest
	if !decodeCardDAV(w, r, &req) {
		return
	}
	result, err := s.cardDAV.Save(r.Context(), req)
	if err != nil {
		s.writeCardDAVAccountError(r.Context(), w, err, "CardDAV discovery or save failed")
		return
	}
	writeJSON(w, 200, result)
}

func (s *Server) writeCardDAVAccountError(
	ctx context.Context, w http.ResponseWriter, err error, message string,
) {
	var statusErr *carddav.StatusError
	switch {
	case errors.Is(err, errCardDAVValidation):
		writeError(w, http.StatusBadRequest, "bad_request", message)
	case errors.Is(err, store.ErrCardDAVCredentialChangePending),
		errors.Is(err, store.ErrCardDAVIdentityChangeOwned):
		writeError(w, http.StatusConflict, "conflict", message)
	case errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusTooManyRequests:
		s.setCardDAVRetryAfterHeader(ctx, w, statusErr.RetryAfter)
		writeError(w, http.StatusServiceUnavailable, "carddav_retry_after", message)
	case errors.Is(err, errCardDAVUpstream):
		writeError(w, http.StatusBadGateway, "carddav_upstream_failed", message)
	default:
		writeError(w, http.StatusInternalServerError, "carddav_storage_failed", message)
	}
}

func (s *Server) writeCardDAVOperationError(
	ctx context.Context, w http.ResponseWriter, err error, message string,
) {
	var statusErr *carddav.StatusError
	var networkErr net.Error
	switch {
	case errors.Is(err, carddav.ErrInvalidResolutionChoice):
		writeError(w, http.StatusBadRequest, "bad_request", message)
	case errors.Is(err, store.ErrCardDAVAddressBookNotFound),
		errors.Is(err, store.ErrCardDAVPublicationNotFound),
		errors.Is(err, store.ErrCardDAVConflictNotFound),
		errors.Is(err, store.ErrPersonNotFound):
		writeError(w, http.StatusNotFound, "not_found", message)
	case errors.Is(err, store.ErrCardDAVStalePlan),
		errors.Is(err, store.ErrCardDAVConflictStale),
		errors.Is(err, store.ErrCardDAVWriteTargetSubscribed),
		errors.Is(err, store.ErrCardDAVReadOnlyAddressBook),
		errors.Is(err, store.ErrCardDAVRoleChangePending),
		errors.Is(err, store.ErrCardDAVPublicationPending),
		errors.Is(err, store.ErrCardDAVPublicationMismatch),
		errors.Is(err, store.ErrCardDAVResourceAmbiguous),
		errors.Is(err, store.ErrCardDAVNoWriteTarget),
		errors.Is(err, carddav.ErrCardDAVConflictPending):
		writeError(w, http.StatusConflict, "conflict", message)
	case errors.Is(err, store.ErrCardDAVRetryAfter):
		s.setCardDAVRetryAfterHeader(ctx, w, 0)
		writeError(w, http.StatusServiceUnavailable, "carddav_retry_after", message)
	case errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusTooManyRequests:
		s.setCardDAVRetryAfterHeader(ctx, w, statusErr.RetryAfter)
		writeError(w, http.StatusServiceUnavailable, "carddav_retry_after", message)
	case errors.As(err, &statusErr), errors.As(err, &networkErr):
		writeError(w, http.StatusBadGateway, "carddav_upstream_failed", message)
	default:
		writeError(w, http.StatusInternalServerError, "carddav_storage_failed", message)
	}
}

func (s *Server) setCardDAVRetryAfterHeader(
	ctx context.Context, w http.ResponseWriter, delay time.Duration,
) {
	if delay <= 0 && s.cardDAV != nil && s.cardDAV.store != nil {
		if gate, err := s.cardDAV.store.GetCardDAVRetryAfterContext(ctx); err == nil && gate != nil {
			delay = time.Until(*gate)
		}
	}
	seconds := max(int64(1), int64((delay+time.Second-1)/time.Second))
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
}
func bookResponse(b store.CardDAVAddressBook) CardDAVBookResponse {
	return CardDAVBookResponse{ID: b.ID, Name: b.DisplayName, URL: b.CanonicalURL, WriteTarget: b.IsWriteTarget, Subscribed: b.IsSubscribed, LookupSource: b.IsLookupSource, NeedsFullReconcile: b.NeedsFullReconcile}
}
func (s *Server) handleCardDAVBooks(w http.ResponseWriter, r *http.Request) {
	svc := s.cardDAVService(w)
	if svc == nil {
		return
	}
	books, err := svc.ListBooks(r.Context())
	if err != nil {
		writeError(w, 500, "carddav_failed", "CardDAV operation failed")
		return
	}
	out := CardDAVBooksResponse{Books: make([]CardDAVBookResponse, 0, len(books))}
	for _, b := range books {
		out.Books = append(out.Books, bookResponse(b))
	}
	writeJSON(w, 200, out)
}
func cardDAVPositivePathID(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("ID must be positive")
	}
	return id, nil
}
func (s *Server) handleCardDAVBookRoles(w http.ResponseWriter, r *http.Request) {
	svc := s.cardDAVService(w)
	if svc == nil {
		return
	}
	id, err := cardDAVPositivePathID(r, "id")
	if err != nil {
		writeError(w, 400, "bad_request", err.Error())
		return
	}
	var req CardDAVBookRolesRequest
	if !decodeCardDAV(w, r, &req) {
		return
	}
	if req.WriteTarget == nil || req.Subscribed == nil || req.LookupSource == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "write_target, subscribed, and lookup_source are required")
		return
	}
	if err = svc.SetBookRoles(r.Context(), id, carddav.BookRoles{WriteTarget: *req.WriteTarget, Subscribed: *req.Subscribed, LookupSource: *req.LookupSource}); err != nil {
		s.writeCardDAVOperationError(r.Context(), w, err, "CardDAV role update failed")
		return
	}
	books, err := svc.ListBooks(r.Context())
	if err != nil {
		s.writeCardDAVOperationError(r.Context(), w, err, "CardDAV book lookup failed")
		return
	}
	for _, b := range books {
		if b.ID == id {
			writeJSON(w, 200, bookResponse(b))
			return
		}
	}
	writeError(w, 404, "not_found", "CardDAV book not found")
}
func publicationResponse(p *store.CardDAVPublication, id int64) CardDAVPublicationResponse {
	if p == nil {
		return CardDAVPublicationResponse{PersonID: id}
	}
	return CardDAVPublicationResponse{PersonID: id, Desired: p.Desired, PendingOperation: string(p.PendingOperation), Href: p.Href}
}
func (s *Server) handleCardDAVPublication(w http.ResponseWriter, r *http.Request) {
	svc := s.cardDAVService(w)
	if svc == nil {
		return
	}
	id, err := cardDAVPositivePathID(r, "person_id")
	if err != nil {
		writeError(w, 400, "bad_request", err.Error())
		return
	}
	p, err := svc.Publication(r.Context(), id)
	if errors.Is(err, store.ErrCardDAVPublicationNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "CardDAV publication not found")
		return
	}
	if err != nil {
		s.writeCardDAVOperationError(r.Context(), w, err, "CardDAV publication lookup failed")
		return
	}
	writeJSON(w, 200, publicationResponse(p, id))
}
func (s *Server) mutatePublication(w http.ResponseWriter, r *http.Request, publish bool) {
	svc := s.cardDAVService(w)
	if svc == nil {
		return
	}
	id, err := cardDAVPositivePathID(r, "person_id")
	if err != nil {
		writeError(w, 400, "bad_request", err.Error())
		return
	}
	if publish {
		err = svc.PublishPerson(r.Context(), id)
	} else {
		err = svc.UnpublishPerson(r.Context(), id)
	}
	if err != nil {
		s.writeCardDAVOperationError(r.Context(), w, err, "CardDAV publication failed")
		return
	}
	p, err := svc.Publication(r.Context(), id)
	if !publish && errors.Is(err, store.ErrCardDAVPublicationNotFound) {
		writeJSON(w, http.StatusOK, CardDAVPublicationResponse{PersonID: id, Desired: false})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "carddav_failed", "CardDAV publication lookup failed")
		return
	}
	writeJSON(w, 200, publicationResponse(p, id))
}
func (s *Server) handleCardDAVPublish(w http.ResponseWriter, r *http.Request) {
	s.mutatePublication(w, r, true)
}
func (s *Server) handleCardDAVUnpublish(w http.ResponseWriter, r *http.Request) {
	s.mutatePublication(w, r, false)
}
func conflictResponse(c store.CardDAVConflict) CardDAVConflictResponse {
	return CardDAVConflictResponse{ID: c.ID, AddressBookID: c.AddressBookID, Href: c.Href, LocalTombstone: c.LocalTombstone, RemoteTombstone: c.RemoteTombstone, Status: string(c.Status)}
}
func conflictDetailResponse(c store.CardDAVConflict) CardDAVConflictDetailResponse {
	return CardDAVConflictDetailResponse{
		ID: c.ID, AddressBookID: c.AddressBookID, Href: c.Href,
		LocalVCard: string(c.LocalBody), RemoteVCard: string(c.RemoteBody),
		LocalTombstone: c.LocalTombstone, RemoteTombstone: c.RemoteTombstone,
		Status: string(c.Status),
	}
}
func (s *Server) handleCardDAVConflicts(w http.ResponseWriter, r *http.Request) {
	svc := s.cardDAVService(w)
	if svc == nil {
		return
	}
	items, err := svc.ListConflicts(r.Context())
	if err != nil {
		writeError(w, 500, "carddav_failed", "CardDAV operation failed")
		return
	}
	out := CardDAVConflictsResponse{Conflicts: make([]CardDAVConflictResponse, 0, len(items))}
	for _, c := range items {
		out.Conflicts = append(out.Conflicts, conflictResponse(c))
	}
	writeJSON(w, 200, out)
}
func (s *Server) handleCardDAVConflict(w http.ResponseWriter, r *http.Request) {
	svc := s.cardDAVService(w)
	if svc == nil {
		return
	}
	id, err := cardDAVPositivePathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	conflict, err := svc.GetConflict(r.Context(), id)
	if err != nil {
		s.writeCardDAVOperationError(r.Context(), w, err, "CardDAV conflict lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, conflictDetailResponse(*conflict))
}
func (s *Server) handleCardDAVResolve(w http.ResponseWriter, r *http.Request) {
	svc := s.cardDAVService(w)
	if svc == nil {
		return
	}
	id, err := cardDAVPositivePathID(r, "id")
	if err != nil {
		writeError(w, 400, "bad_request", err.Error())
		return
	}
	var req CardDAVResolveRequest
	if !decodeCardDAV(w, r, &req) {
		return
	}
	if err = svc.ResolveConflict(r.Context(), id, req.Choice); err != nil {
		s.writeCardDAVOperationError(r.Context(), w, err, "CardDAV conflict resolution failed")
		return
	}
	writeJSON(w, 200, CardDAVConflictResolutionResponse{ID: id, Status: string(store.CardDAVConflictResolved)})
}
func (s *Server) handleCardDAVSync(w http.ResponseWriter, r *http.Request) {
	svc := s.cardDAVService(w)
	if svc == nil {
		return
	}
	var req CardDAVSyncRequest
	if !decodeCardDAV(w, r, &req) {
		return
	}
	result, err := svc.Sync(r.Context(), carddav.SyncOptions{Full: req.Full})
	if err != nil {
		s.writeCardDAVOperationError(r.Context(), w, err, "CardDAV synchronization failed")
		return
	}
	writeJSON(w, 200, result)
}
