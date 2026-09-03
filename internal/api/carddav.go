package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	errCardDAVValidation  = errors.New("invalid CardDAV request")
	errCardDAVUpstream    = errors.New("CardDAV upstream failure")
	errCardDAVStorage     = errors.New("CardDAV storage failure")
	errCardDAVUnavailable = errors.New("CardDAV status unavailable")
)

type CardDAVOperations interface {
	Sync(ctx context.Context, options carddav.SyncOptions) (carddav.SyncResult, error)
	ListBooks(ctx context.Context) ([]store.CardDAVAddressBook, error)
	SetBookRoles(ctx context.Context, bookID int64, roles carddav.BookRoles) error
	PublicationView(ctx context.Context, personID int64) (*carddav.PublicationView, error)
	PublishPerson(ctx context.Context, personID int64) error
	UnpublishPerson(ctx context.Context, personID int64) error
	ListConflictViews(ctx context.Context) ([]carddav.ConflictListItem, error)
	GetConflictView(ctx context.Context, conflictID int64) (*carddav.ConflictDetail, error)
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
	mu                sync.RWMutex
	saveMu            sync.Mutex
	cfg               *config.Config
	store             *store.Store
	service           CardDAVOperations
	factory           cardDAVServiceFactory
	persistDiscovery  func(context.Context, cardDAVCandidate, string, string, carddav.Discovery, bool) error
	saveConfig        func(*config.CardDAVConfig, config.CardDAVConfig) (config.CardDAVConfig, error)
	saveCredential    func(string, carddav.Credential) error
	loadCredential    func(string) (carddav.Credential, error)
	removeCredential  func(string) error
	reconcileSchedule func(config.CardDAVConfig, CardDAVOperations) error
}

// SetScheduleReconciler wires the daemon's live scheduler into successful
// account saves. The callback receives the newly persisted config and service.
func (c *CardDAVController) SetScheduleReconciler(reconcile func(config.CardDAVConfig, CardDAVOperations) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reconcileSchedule = reconcile
}

// NewCardDAVController loads the saved account and credential. A credential
// that cannot be read leaves CardDAV unavailable for repair rather than
// failing daemon startup; the cause is logged so the operator can see why.
func NewCardDAVController(cfg *config.Config, st *store.Store, logger *slog.Logger) (*CardDAVController, error) {
	if logger == nil {
		return nil, errors.New("CardDAV controller requires a logger")
	}
	c := &CardDAVController{cfg: cfg, store: st, factory: newCardDAVService}
	c.saveCredential = carddav.SaveCredential
	c.loadCredential = carddav.LoadCredential
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
			logger.Warn("CardDAV legacy credential is unreadable; CardDAV stays unavailable until the account is repaired",
				"error", legacyErr)
			return c, nil
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
		logger.Warn("CardDAV credential is unreadable; CardDAV stays unavailable until the account is repaired",
			"error", err)
		return c, nil
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
	next := config.CardDAVConfig{
		BaseURL: req.BaseURL, Username: req.Username, Enabled: *req.Enabled, Schedule: req.Schedule,
	}
	current := c.cardDAVConfigSnapshot()
	identityChanged := current.BaseURL != next.BaseURL || current.Username != next.Username
	enabling := !current.Enabled && next.Enabled
	if req.Password == "" && !identityChanged && !enabling {
		return c.saveCardDAVConfigOnly(ctx, next)
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
	var previousCredentialFile carddav.CredentialFileSnapshot
	hadPreviousCredentialFile := false
	if req.Password != "" && previousCredentialErr != nil {
		previousCredentialFile, err = carddav.CaptureCredentialFile(tokenDir)
		if err != nil {
			return CardDAVAccountResponse{}, errors.Join(errCardDAVStorage, err)
		}
		hadPreviousCredentialFile = true
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
		if hadPreviousCredentialFile {
			return previousCredentialFile.Restore(tokenDir)
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

func (c *CardDAVController) saveCardDAVConfigOnly(
	ctx context.Context, next config.CardDAVConfig,
) (CardDAVAccountResponse, error) {
	books, err := c.store.ListCardDAVAddressBooksContext(ctx)
	if err != nil {
		return CardDAVAccountResponse{}, errors.Join(errCardDAVStorage, err)
	}
	c.mu.RLock()
	service := c.service
	reconcileSchedule := c.reconcileSchedule
	c.mu.RUnlock()
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
		BaseURL: next.BaseURL, Username: next.Username, Enabled: next.Enabled,
		Schedule: next.Schedule, Books: len(books),
	}, nil
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
	PersonID         int64                               `json:"person_id" minimum:"1"`
	State            carddav.PublicationState            `json:"state" enum:"unpublished,published,pending,conflict"`
	Desired          bool                                `json:"desired"`
	PendingOperation store.CardDAVMutationOperation      `json:"pending_operation,omitempty" enum:"create,update,delete"`
	AddressBook      *CardDAVAddressBookIdentityResponse `json:"address_book,omitempty"`
	ConflictID       *int64                              `json:"conflict_id,omitempty" minimum:"1"`
}
type CardDAVAddressBookIdentityResponse struct {
	ID   int64  `json:"id" minimum:"1"`
	Name string `json:"name"`
}
type CardDAVContactSummaryResponse struct {
	State       carddav.ConflictSideState `json:"state" enum:"present,deleted,unavailable"`
	DisplayName string                    `json:"display_name,omitempty"`
	Emails      []string                  `json:"emails" nullable:"false"`
	Phones      []string                  `json:"phones" nullable:"false"`
	Truncated   bool                      `json:"truncated,omitempty"`
}
type CardDAVConflictResponse struct {
	ID                 int64                              `json:"id" minimum:"1"`
	AddressBook        CardDAVAddressBookIdentityResponse `json:"address_book"`
	Status             store.CardDAVConflictStatus        `json:"status" enum:"unresolved,resolved"`
	LocalState         carddav.ConflictSideState          `json:"local_state" enum:"present,deleted,unavailable"`
	RemoteState        carddav.ConflictSideState          `json:"remote_state" enum:"present,deleted,unavailable"`
	AllowedResolutions []carddav.ResolutionChoice         `json:"allowed_resolutions" enum:"keep_local,keep_remote" nullable:"false"`
	UpdatedAt          time.Time                          `json:"updated_at"`
}
type CardDAVConflictDetailResponse struct {
	ID                 int64                              `json:"id" minimum:"1"`
	AddressBook        CardDAVAddressBookIdentityResponse `json:"address_book"`
	Status             store.CardDAVConflictStatus        `json:"status" enum:"unresolved,resolved"`
	Resolution         store.CardDAVConflictResolution    `json:"resolution,omitempty" enum:"keep_local,keep_remote"`
	Base               CardDAVContactSummaryResponse      `json:"base"`
	Local              CardDAVContactSummaryResponse      `json:"local"`
	Remote             CardDAVContactSummaryResponse      `json:"remote"`
	AllowedResolutions []carddav.ResolutionChoice         `json:"allowed_resolutions" enum:"keep_local,keep_remote" nullable:"false"`
	CreatedAt          time.Time                          `json:"created_at"`
	UpdatedAt          time.Time                          `json:"updated_at"`
	ResolvedAt         *time.Time                         `json:"resolved_at,omitempty"`
}
type CardDAVConflictResolutionResponse struct {
	ID         int64                       `json:"id" minimum:"1"`
	Status     store.CardDAVConflictStatus `json:"status" enum:"resolved"`
	Resolution carddav.ResolutionChoice    `json:"resolution" enum:"keep_local,keep_remote"`
}
type CardDAVConflictsResponse struct {
	Conflicts []CardDAVConflictResponse `json:"conflicts" nullable:"false"`
}
type CardDAVResolveRequest struct {
	Choice carddav.ResolutionChoice `json:"choice" enum:"keep_local,keep_remote"`
}
type CardDAVSyncRequest struct {
	Full bool `json:"full,omitempty"`
}

type CardDAVRunResponse struct {
	ID           int64      `json:"id"`
	Trigger      string     `json:"trigger" enum:"manual,scheduled"`
	Full         bool       `json:"full"`
	State        string     `json:"state" enum:"running,succeeded,failed,cancelled,partial"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	Books        int64      `json:"books"`
	Created      int64      `json:"created"`
	Updated      int64      `json:"updated"`
	Removed      int64      `json:"removed"`
	ErrorCode    string     `json:"error_code,omitempty" enum:"cancelled,retry_after,authentication_failed,upstream_failed,safety_limit,sync_failed,unsafe_error_redacted,daemon_restarted"`
	ErrorMessage string     `json:"error_message,omitempty"`
}

type CardDAVStatusAccount struct {
	BaseURL  string `json:"base_url"`
	Username string `json:"username"`
}

type CardDAVStatusResponse struct {
	Configured           bool                  `json:"configured"`
	Available            bool                  `json:"available"`
	CredentialConfigured bool                  `json:"credential_configured"`
	Enabled              bool                  `json:"enabled"`
	Scheduled            bool                  `json:"scheduled"`
	Schedule             string                `json:"schedule"`
	NextScheduledAt      *time.Time            `json:"next_scheduled_at,omitempty"`
	RepairReason         string                `json:"repair_reason,omitempty" enum:"account_missing,credential_missing,credential_mismatch,credential_unavailable,runtime_unavailable"`
	Account              *CardDAVStatusAccount `json:"account,omitempty"`
	Active               *CardDAVRunResponse   `json:"active,omitempty"`
	Latest               *CardDAVRunResponse   `json:"latest,omitempty"`
	LatestSuccessful     *CardDAVRunResponse   `json:"latest_successful,omitempty"`
}

type CardDAVRunsResponse struct {
	Runs         []CardDAVRunResponse `json:"runs" nullable:"false"`
	NextBeforeID *int64               `json:"next_before_id,omitempty"`
}

func cardDAVRunResponse(run *store.CardDAVSyncRun) *CardDAVRunResponse {
	if run == nil {
		return nil
	}
	errorCode, errorMessage := cardDAVRunPublicFailure(run.ErrorCode)
	return &CardDAVRunResponse{
		ID: run.ID, Trigger: string(run.Trigger), Full: run.Full, State: string(run.State),
		StartedAt: run.StartedAt, FinishedAt: run.FinishedAt,
		Books: run.Books, Created: run.Created, Updated: run.Updated, Removed: run.Removed,
		ErrorCode: errorCode, ErrorMessage: errorMessage,
	}
}

func cardDAVRunPublicFailure(code string) (string, string) {
	switch code {
	case "":
		return "", ""
	case "cancelled":
		return code, "CardDAV sync was cancelled."
	case "retry_after":
		return code, "CardDAV sync is temporarily paused."
	case "authentication_failed":
		return code, "CardDAV authentication failed."
	case "upstream_failed":
		return code, "CardDAV server request failed."
	case "safety_limit":
		return code, "CardDAV sync exceeded its safety limits."
	case "sync_failed":
		return code, "CardDAV sync failed."
	case "unsafe_error_redacted":
		return code, "CardDAV sync failed; sensitive details were removed."
	case "daemon_restarted":
		return code, "CardDAV sync stopped because the daemon restarted."
	default:
		return "sync_failed", "CardDAV sync failed."
	}
}

func (c *CardDAVController) Status(ctx context.Context) (CardDAVStatusResponse, error) {
	if c == nil || c.store == nil || c.cfg == nil {
		return CardDAVStatusResponse{}, errCardDAVUnavailable
	}
	c.mu.RLock()
	cfg := c.cfg.CardDAV
	service := c.service
	loadCredential := c.loadCredential
	c.mu.RUnlock()
	status := CardDAVStatusResponse{Schedule: cfg.Schedule}
	status.Enabled = cfg.Enabled
	status.Available = service != nil
	status.Configured = strings.TrimSpace(cfg.BaseURL) != "" && strings.TrimSpace(cfg.Username) != ""
	if status.Configured {
		status.Account = &CardDAVStatusAccount{BaseURL: cardDAVStatusBaseURL(cfg.BaseURL), Username: cfg.Username}
	}
	runs, err := c.store.CardDAVSyncStatusContext(ctx)
	if err != nil {
		return CardDAVStatusResponse{}, errors.Join(errCardDAVStorage, err)
	}
	status.Active = cardDAVRunResponse(runs.Active)
	status.Latest = cardDAVRunResponse(runs.Latest)
	status.LatestSuccessful = cardDAVRunResponse(runs.LatestSuccessful)
	if !status.Configured {
		return status, nil
	}
	account, err := c.store.GetCardDAVAccountContext(ctx)
	if err != nil {
		return CardDAVStatusResponse{}, errors.Join(errCardDAVStorage, err)
	}
	if account == nil {
		status.RepairReason = "account_missing"
		return status, nil
	}
	if loadCredential == nil {
		loadCredential = carddav.LoadCredential
	}
	credential, err := loadCredential(c.cfg.TokensDir())
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, carddav.ErrCredentialNotBound) {
		status.RepairReason = "credential_missing"
		return status, nil
	}
	if err != nil {
		status.RepairReason = "credential_unavailable"
		return status, nil //nolint:nilerr // Status reports the recoverable credential condition.
	}
	status.CredentialConfigured = credential.BaseURL == cfg.BaseURL && credential.Username == cfg.Username &&
		credential.BaseURL == account.BaseURL && credential.Username == account.Username &&
		credential.ConnectionGeneration == account.ConnectionGeneration
	if !status.CredentialConfigured {
		status.RepairReason = "credential_mismatch"
	} else if !status.Available {
		status.RepairReason = "runtime_unavailable"
	}
	return status, nil
}

func cardDAVStatusBaseURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.RawFragment = ""
	return parsed.String()
}

func (c *CardDAVController) Runs(ctx context.Context, limit int, beforeID *int64) (CardDAVRunsResponse, error) {
	if c == nil || c.store == nil {
		return CardDAVRunsResponse{}, errCardDAVUnavailable
	}
	runs, err := c.store.ListCardDAVSyncRunsContext(ctx, limit, beforeID)
	if err != nil {
		return CardDAVRunsResponse{}, errors.Join(errCardDAVStorage, err)
	}
	result := CardDAVRunsResponse{Runs: make([]CardDAVRunResponse, 0, len(runs))}
	for i := range runs {
		result.Runs = append(result.Runs, *cardDAVRunResponse(&runs[i]))
	}
	if len(runs) == limit && len(runs) > 0 {
		next := runs[len(runs)-1].ID
		result.NextBeforeID = &next
	}
	return result, nil
}

func (s *Server) registerCardDAVRoutes(api huma.API) {
	registerCardDAVJSONRoute[CardDAVStatusResponse](api, "getCardDAVStatus", http.MethodGet, "/carddav/status", "Get CardDAV synchronization status", s.handleCardDAVStatus, http.StatusInternalServerError, http.StatusServiceUnavailable)
	runs := rawAPIV1Operation("listCardDAVRuns", http.MethodGet, "/carddav/runs", "List CardDAV synchronization runs")
	limit := queryIntegerParam("limit", "Maximum runs to return (default 25, max 100)")
	minimum, maximum := float64(1), float64(100)
	limit.Schema.Minimum, limit.Schema.Maximum = &minimum, &maximum
	before := queryIntegerParam("before_id", "Return runs with IDs lower than this cursor")
	before.Schema.Minimum = &minimum
	runs.Parameters = append(runs.Parameters, limit, before)
	runs.Responses = jsonResponsesFor[CardDAVRunsResponse](api)
	addErrorResponses(api, runs.Responses, http.StatusBadRequest, http.StatusInternalServerError, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, runs, s.handleCardDAVRuns)
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

func (s *Server) handleCardDAVStatus(w http.ResponseWriter, r *http.Request) {
	if s.cardDAV == nil {
		writeError(w, http.StatusServiceUnavailable, "carddav_unavailable", "CardDAV status is unavailable")
		return
	}
	status, err := s.cardDAV.Status(r.Context())
	if err != nil {
		if errors.Is(err, errCardDAVUnavailable) {
			writeError(w, http.StatusServiceUnavailable, "carddav_unavailable", "CardDAV status is unavailable")
		} else {
			writeError(w, http.StatusInternalServerError, "carddav_storage_failed", "CardDAV status lookup failed")
		}
		return
	}
	if s.scheduler != nil && s.scheduler.IsRunning() {
		for _, job := range s.scheduler.JobStatus() {
			if job.Name != CardDAVJobName {
				continue
			}
			status.Scheduled = true
			if !job.NextRun.IsZero() {
				next := job.NextRun
				status.NextScheduledAt = &next
			}
			break
		}
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleCardDAVRuns(w http.ResponseWriter, r *http.Request) {
	if s.cardDAV == nil {
		writeError(w, http.StatusServiceUnavailable, "carddav_unavailable", "CardDAV run history is unavailable")
		return
	}
	limit := 25
	if parsed, present, err := queryInt(r, "limit"); err != nil {
		s.rejectBadParam(w, err)
		return
	} else if present {
		if parsed < 1 || parsed > 100 {
			s.rejectBadParam(w, newParamError("limit", "query parameter \"limit\" must be between 1 and 100"))
			return
		}
		limit = parsed
	}
	var beforeID *int64
	if parsed, present, err := queryInt64(r, "before_id"); err != nil {
		s.rejectBadParam(w, err)
		return
	} else if present {
		if parsed <= 0 {
			s.rejectBadParam(w, newParamError("before_id", "query parameter \"before_id\" must be positive"))
			return
		}
		beforeID = &parsed
	}
	result, err := s.cardDAV.Runs(r.Context(), limit, beforeID)
	if err != nil {
		if errors.Is(err, errCardDAVUnavailable) {
			writeError(w, http.StatusServiceUnavailable, "carddav_unavailable", "CardDAV run history is unavailable")
		} else {
			writeError(w, http.StatusInternalServerError, "carddav_storage_failed", "CardDAV run history lookup failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, result)
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
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
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
	case errors.Is(err, store.ErrCardDAVConflictStale):
		writeError(w, http.StatusConflict, "carddav_conflict_stale", "CardDAV conflict changed; refresh before trying again")
	case errors.Is(err, carddav.ErrCardDAVConflictPending):
		writeError(w, http.StatusConflict, "carddav_conflict_pending", "Resolve the existing CardDAV conflict before trying again")
	case errors.Is(err, store.ErrCardDAVPublicationPending):
		writeError(w, http.StatusConflict, "carddav_publication_pending", "CardDAV publication is pending; refresh before trying again")
	case errors.Is(err, store.ErrCardDAVStalePlan),
		errors.Is(err, store.ErrCardDAVSyncActive),
		errors.Is(err, store.ErrCardDAVWriteTargetSubscribed),
		errors.Is(err, store.ErrCardDAVReadOnlyAddressBook),
		errors.Is(err, store.ErrCardDAVRoleChangePending),
		errors.Is(err, store.ErrCardDAVPublicationMismatch),
		errors.Is(err, store.ErrCardDAVResourceAmbiguous),
		errors.Is(err, store.ErrCardDAVNoWriteTarget):
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
func addressBookIdentityResponse(book carddav.AddressBookIdentity) CardDAVAddressBookIdentityResponse {
	return CardDAVAddressBookIdentityResponse{ID: book.ID, Name: book.Name}
}
func publicationResponse(view *carddav.PublicationView) CardDAVPublicationResponse {
	response := CardDAVPublicationResponse{
		PersonID: view.PersonID, State: view.State, Desired: view.Desired,
		PendingOperation: view.PendingOperation, ConflictID: view.ConflictID,
	}
	if view.AddressBook != nil {
		book := addressBookIdentityResponse(*view.AddressBook)
		response.AddressBook = &book
	}
	return response
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
	p, err := svc.PublicationView(r.Context(), id)
	if err != nil {
		s.writeCardDAVOperationError(r.Context(), w, err, "CardDAV publication lookup failed")
		return
	}
	writeJSON(w, 200, publicationResponse(p))
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
	p, err := svc.PublicationView(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "carddav_failed", "CardDAV publication lookup failed")
		return
	}
	writeJSON(w, 200, publicationResponse(p))
}
func (s *Server) handleCardDAVPublish(w http.ResponseWriter, r *http.Request) {
	s.mutatePublication(w, r, true)
}
func (s *Server) handleCardDAVUnpublish(w http.ResponseWriter, r *http.Request) {
	s.mutatePublication(w, r, false)
}
func conflictResponse(c carddav.ConflictListItem) CardDAVConflictResponse {
	return CardDAVConflictResponse{
		ID: c.ID, AddressBook: addressBookIdentityResponse(c.AddressBook), Status: c.Status,
		LocalState: c.LocalState, RemoteState: c.RemoteState,
		AllowedResolutions: c.AllowedResolutions, UpdatedAt: c.UpdatedAt,
	}
}
func contactSummaryResponse(summary carddav.ContactSummary) CardDAVContactSummaryResponse {
	return CardDAVContactSummaryResponse{
		State: summary.State, DisplayName: summary.DisplayName, Emails: summary.Emails,
		Phones: summary.Phones, Truncated: summary.Truncated,
	}
}
func conflictDetailResponse(c carddav.ConflictDetail) CardDAVConflictDetailResponse {
	return CardDAVConflictDetailResponse{
		ID: c.ID, AddressBook: addressBookIdentityResponse(c.AddressBook), Status: c.Status,
		Resolution: c.Resolution, Base: contactSummaryResponse(c.Base),
		Local: contactSummaryResponse(c.Local), Remote: contactSummaryResponse(c.Remote),
		AllowedResolutions: c.AllowedResolutions, CreatedAt: c.CreatedAt,
		UpdatedAt: c.UpdatedAt, ResolvedAt: c.ResolvedAt,
	}
}
func (s *Server) handleCardDAVConflicts(w http.ResponseWriter, r *http.Request) {
	svc := s.cardDAVService(w)
	if svc == nil {
		return
	}
	items, err := svc.ListConflictViews(r.Context())
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
	conflict, err := svc.GetConflictView(r.Context(), id)
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
	writeJSON(w, 200, CardDAVConflictResolutionResponse{ID: id, Status: store.CardDAVConflictResolved, Resolution: req.Choice})
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
	result, err := svc.Sync(r.Context(), carddav.SyncOptions{
		Full: req.Full, Trigger: store.CardDAVSyncTriggerManual,
	})
	if err != nil {
		s.writeCardDAVOperationError(r.Context(), w, err, "CardDAV synchronization failed")
		return
	}
	writeJSON(w, 200, result)
}
