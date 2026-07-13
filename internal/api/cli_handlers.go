package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/accountops"
	"go.kenn.io/msgvault/internal/cacheops"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/collectionops"
	"go.kenn.io/msgvault/internal/deletion"
	msgexport "go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/identityops"
	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

// CLIStore exposes archive operations needed by CLI-compatible HTTP routes.
// These routes preserve local CLI output contracts while keeping SQLite access
// inside the daemon process.
type CLIStore interface {
	GetStatsForScope(sourceIDs []int64) (*store.Stats, error)
	GetSourcesByIdentifierOrDisplayName(query string) ([]*store.Source, error)
	GetSourcesByTypeAndAccount(sourceType, accountEmail string) ([]*store.Source, error)
	GetCollectionByName(name string) (*store.CollectionWithSources, error)
	ListCollections() ([]*store.CollectionWithSources, error)
	CreateCollection(name, description string, sourceIDs []int64) (*store.Collection, error)
	AddSourcesToCollection(name string, sourceIDs []int64) error
	RemoveSourcesFromCollection(name string, sourceIDs []int64) error
	DeleteCollection(name string) error
	UpdateSourceDisplayName(sourceID int64, displayName string) error
	ListSources(sourceType string) ([]*store.Source, error)
	GetSourceByID(id int64) (*store.Source, error)
	ListAccountIdentities(sourceID int64) ([]store.AccountIdentity, error)
	AddAccountIdentity(sourceID int64, address, signal string) error
	RemoveAccountIdentity(sourceID int64, address string) (int64, error)
	CountMessagesForSource(sourceID int64) (int64, error)
	CountSourceDeletedMessages(sourceIDs ...int64) (int64, error)
	NeedsFTSBackfill() bool
	NeedsFTSBackfillQuick() bool
	BackfillFTS(progress func(done, total int64)) (int64, error)
	RebuildFTS(progress func(done, total int64)) (int64, error)
}

// CLIStartupMigrationStore exposes one-time startup migrations needed by
// setup-style CLI commands while keeping writes inside the daemon process.
type CLIStartupMigrationStore interface {
	RunStartupMigrations(legacyIdentityAddresses []string) (store.StartupMigrationResult, error)
}

// CLICacheBuilder runs the user-facing analytics cache build while streaming
// CLI output back to the HTTP caller.
type CLICacheBuilder interface {
	BuildCLICache(ctx context.Context, fullRebuild bool, emit func(CLICacheBuildEvent) error) error
}

type CLISyncRunner interface {
	RunCLISync(ctx context.Context, req CLISyncRequest, emit func(CLISyncEvent) error) error
}

type CLIVerifyRunner interface {
	RunCLIVerify(ctx context.Context, req CLIVerifyRequest, emit func(CLIVerifyEvent) error) error
}

type CLIRunner interface {
	RunCLICommand(ctx context.Context, req CLIRunRequest, emit func(CLIRunEvent) error) error
}

type CLIAddCalendarPlanner interface {
	PlanCLIAddCalendar(ctx context.Context, req CLIAddCalendarPlanRequest) (CLIAddCalendarPlanResponse, error)
}

type CLIEmbeddingsPlanner interface {
	PlanCLIEmbeddings(ctx context.Context, req CLIEmbeddingsPlanRequest) (CLIEmbeddingsPlanResponse, error)
}

type CLIDeleteStagedPlanner interface {
	PlanCLIDeleteStaged(ctx context.Context, req CLIDeleteStagedPlanRequest) (CLIDeleteStagedPlanResponse, error)
}

type CLIDeletionManifestSaver interface {
	SaveCLIDeletionManifest(ctx context.Context, manifest *deletion.Manifest) error
}

type CLIDeduplicatePlanner interface {
	PlanCLIDeduplicate(ctx context.Context, req CLIDeduplicatePlanRequest) (CLIDeduplicatePlanResponse, error)
}

const (
	cliStreamEventTypeComplete = "complete"
	cliStreamEventTypeError    = "error"
)

type CLIRepairEncodingRunner interface {
	RunCLIRepairEncoding(ctx context.Context, emit func(CLIRepairEncodingEvent) error) error
}

// CLIDedupDeleteStore exposes the destructive dedup-delete operations used by
// the CLI-compatible HTTP routes.
type CLIDedupDeleteStore interface {
	CountAllDeduped() (int64, int64, error)
	CountDedupedBatches(batchIDs []string) ([]store.DedupedBatchCount, int64, error)
	DeleteAllDeduped() (int64, int64, error)
	DeleteDedupedBatch(batchID string) (int64, error)
	BackupDatabase(dst string) error
}

func (s *Server) cliStore() (CLIStore, *apiHTTPError) {
	cliStore, ok := s.store.(CLIStore)
	if !ok {
		return nil, cliStoreUnavailableError()
	}
	return cliStore, nil
}

func cliStoreUnavailableError() *apiHTTPError {
	return newAPIHTTPError(
		http.StatusServiceUnavailable,
		"cli_store_unavailable",
		"CLI archive operations are not available",
	)
}

func cliDedupStoreUnavailableError() *apiHTTPError {
	return newAPIHTTPError(
		http.StatusServiceUnavailable,
		"cli_dedup_store_unavailable",
		"CLI dedup delete operations are not available",
	)
}

type cliStatsResponse struct {
	Stats            StatsResponse `json:"stats"`
	ScopeLabel       string        `json:"scope_label,omitempty"`
	ScopeSourceCount int           `json:"scope_source_count,omitempty"`
}

type cliInitDBResponse struct {
	Stats  StatsResponse `json:"stats"`
	Notice string        `json:"notice,omitempty"`
}

type cliCacheStatsResponse = cacheops.CacheStats

type CLICacheBuildEvent struct {
	Type  string `json:"type"`
	Data  string `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

type CLISyncRequest struct {
	Full     bool
	Email    string
	Query    string
	NoResume bool
	Before   string
	After    string
	Limit    int
}

type CLISyncEvent struct {
	Type  string `json:"type"`
	Data  string `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

type CLIVerifyRequest struct {
	Email       string
	SampleSize  int
	SkipDBCheck bool
	JSON        bool
}

type CLIVerifyEvent struct {
	Type  string `json:"type"`
	Data  string `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

type CLIRepairEncodingEvent struct {
	Type  string `json:"type"`
	Data  string `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

type CLIRunRequest struct {
	Args []string          `json:"args"`
	Env  map[string]string `json:"env,omitempty"`
	Cwd  string            `json:"cwd,omitempty"`
}

type CLIAddCalendarPlanRequest struct {
	Email            string `json:"email"`
	OAuthApp         string `json:"oauth_app,omitempty"`
	OAuthAppExplicit bool   `json:"oauth_app_explicit,omitempty"`
	Headless         bool   `json:"headless,omitempty"`
}

type CLIAddCalendarPlanResponse struct {
	NeedsScopeEscalation bool     `json:"needs_scope_escalation"`
	Headline             string   `json:"headline,omitempty"`
	BodyLines            []string `json:"body_lines,omitempty"`
	CancelHint           string   `json:"cancel_hint,omitempty"`
	// OAuthApp and NeedsClientCheck let the frontend CLI run browser
	// authorization client-side before proxying: the resolved OAuth app
	// binding and whether the stored token must match that app's client.
	// OAuthAppResolved is always true from daemons that resolve the
	// binding; false marks an older daemon whose stored named app is
	// unknown, so the frontend must not authorize with the default app.
	OAuthApp         string `json:"oauth_app,omitempty"`
	OAuthAppResolved bool   `json:"oauth_app_resolved,omitempty"`
	NeedsClientCheck bool   `json:"needs_client_check,omitempty"`
}

type CLIEmbeddingsPlanRequest struct {
	Operation    string `json:"operation"`
	GenerationID int64  `json:"generation_id"`
	Force        bool   `json:"force,omitempty"`
}

type CLIEmbeddingsPlanResponse struct {
	NeedsConfirmation bool   `json:"needs_confirmation"`
	Prompt            string `json:"prompt,omitempty"`
}

type CLIDeleteStagedPlanRequest struct {
	BatchID             string `json:"batch_id,omitempty"`
	Permanent           bool   `json:"permanent,omitempty"`
	Yes                 bool   `json:"yes,omitempty"`
	DryRun              bool   `json:"dry_run,omitempty"`
	List                bool   `json:"list,omitempty"`
	Account             string `json:"account,omitempty"`
	RemoteDeleteEnabled bool   `json:"remote_delete_enabled,omitempty"`
}

type CLIDeleteStagedPlanResponse struct {
	Stdout                    string   `json:"stdout,omitempty"`
	NeedsExecution            bool     `json:"needs_execution"`
	NeedsConfirmation         bool     `json:"needs_confirmation"`
	ConfirmationMode          string   `json:"confirmation_mode,omitempty"`
	PlannedBatchIDs           []string `json:"planned_batch_ids,omitempty"`
	PlanFingerprint           string   `json:"plan_fingerprint,omitempty"`
	NeedsScopeEscalation      bool     `json:"needs_scope_escalation,omitempty"`
	ScopeEscalationHeadline   string   `json:"scope_escalation_headline,omitempty"`
	ScopeEscalationBodyLines  []string `json:"scope_escalation_body_lines,omitempty"`
	ScopeEscalationCancelHint string   `json:"scope_escalation_cancel_hint,omitempty"`
	// ScopeEscalationAccount and ScopeEscalationOAuthApp let the frontend
	// CLI run the confirmed scope-upgrade authorization client-side before
	// proxying, instead of opening a browser in the daemon subprocess.
	ScopeEscalationAccount  string `json:"scope_escalation_account,omitempty"`
	ScopeEscalationOAuthApp string `json:"scope_escalation_oauth_app,omitempty"`
	BlockedError            string `json:"blocked_error,omitempty"`
	RemoteDeleteEnvVar      string `json:"remote_delete_env_var,omitempty"`
}

type CLIDeletionManifestResponse struct {
	ID           string `json:"id"`
	MessageCount int    `json:"message_count"`
}

type CLIDeduplicatePlanRequest struct {
	Account                    string `json:"account,omitempty"`
	Collection                 string `json:"collection,omitempty"`
	Prefer                     string `json:"prefer,omitempty"`
	ContentHash                bool   `json:"content_hash,omitempty"`
	DeleteDupsFromSourceServer bool   `json:"delete_dups_from_source_server,omitempty"`
}

type CLIDeduplicatePlanResponse struct {
	PrefixStdout string                   `json:"prefix_stdout,omitempty"`
	Items        []CLIDeduplicatePlanItem `json:"items"`
	FooterStdout string                   `json:"footer_stdout,omitempty"`
}

type CLIDeduplicatePlanItem struct {
	SourceID          int64  `json:"source_id,omitempty"`
	ScopeLabel        string `json:"scope_label,omitempty"`
	ScopeIsCollection bool   `json:"scope_is_collection,omitempty"`
	Stdout            string `json:"stdout,omitempty"`
	DuplicateMessages int    `json:"duplicate_messages,omitempty"`
	BackfilledCount   int64  `json:"backfilled_count,omitempty"`
	PlanFingerprint   string `json:"plan_fingerprint,omitempty"`
	NeedsConfirmation bool   `json:"needs_confirmation"`
}

type CLIRunEvent struct {
	Type  string `json:"type"`
	Data  string `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

type cliDeleteDedupedScopeRequest struct {
	BatchIDs  []string `json:"batch_ids,omitempty"`
	AllHidden bool     `json:"all_hidden,omitempty"`
}

type cliDeleteDedupedPlanRequest struct {
	BatchIDs  []string `json:"batch_ids,omitempty"`
	AllHidden bool     `json:"all_hidden,omitempty"`
}

func (r cliDeleteDedupedPlanRequest) scope() cliDeleteDedupedScopeRequest {
	return cliDeleteDedupedScopeRequest(r)
}

type cliDeleteDedupedExecuteRequest struct {
	BatchIDs           []string                        `json:"batch_ids,omitempty"`
	AllHidden          bool                            `json:"all_hidden,omitempty"`
	NoBackup           bool                            `json:"no_backup,omitempty"`
	ExpectedTotal      *int64                          `json:"expected_total" nullable:"false"`
	ExpectedBatchCount *int64                          `json:"expected_batch_count" nullable:"false"`
	ExpectedBatches    []cliDeleteDedupedBatchResponse `json:"expected_batches" nullable:"false"`
}

func (r cliDeleteDedupedExecuteRequest) scope() cliDeleteDedupedScopeRequest {
	return cliDeleteDedupedScopeRequest{
		BatchIDs:  r.BatchIDs,
		AllHidden: r.AllHidden,
	}
}

type cliDeleteDedupedBatchResponse struct {
	ID    string `json:"id"`
	Count int64  `json:"count"`
}

type cliDeleteDedupedPlanResponse struct {
	Total      int64                           `json:"total"`
	BatchCount int64                           `json:"batch_count"`
	Batches    []cliDeleteDedupedBatchResponse `json:"batches,omitempty"`
}

type cliDeleteDedupedExecuteResponse struct {
	Deleted    int64  `json:"deleted"`
	BatchCount int64  `json:"batch_count"`
	BackupPath string `json:"backup_path,omitempty"`
}

type cliSearchResponse struct {
	Results          []query.MessageSummary `json:"results"`
	ScopeLabel       string                 `json:"scope_label,omitempty"`
	ScopeSourceCount int                    `json:"scope_source_count,omitempty"`
	// IndexBuilt/IndexedMessages are only set by daemons that built the FTS
	// index synchronously inside the request (pre-0.18); current daemons
	// build in the background and report IndexState instead. Kept in the
	// schema so new CLIs understand old daemons.
	IndexBuilt      bool  `json:"index_built,omitempty"`
	IndexedMessages int64 `json:"indexed_messages,omitempty"`
	// IndexState is "checking" while the FTS completeness probe runs and
	// "building" while a backfill is repopulating the index (results may be
	// incomplete); empty once the index is known complete.
	IndexState string `json:"index_state,omitempty"`
}

// CLI search index states reported in cliSearchResponse.IndexState.
const (
	cliSearchIndexStateChecking = "checking"
	cliSearchIndexStateBuilding = "building"
)

type CLIQueryMessageSummary query.MessageSummary

type cliAccountsResponse struct {
	Accounts []cliAccountResponse `json:"accounts"`
}

type cliCollectionsResponse struct {
	Collections []cliCollectionResponse `json:"collections"`
}

type cliCollectionEnvelope struct {
	Collection cliCollectionResponse `json:"collection"`
}

type cliIdentitiesResponse struct {
	Rows []cliIdentityRowResponse `json:"rows"`
}

type cliIdentityAddResponse = identityops.AddResult
type cliIdentityRemoveResponse = identityops.RemoveResult

type cliRebuildFTSEvent struct {
	Type    string `json:"type"`
	Done    int64  `json:"done,omitempty"`
	Total   int64  `json:"total,omitempty"`
	Indexed int64  `json:"indexed,omitempty"`
	Error   string `json:"error,omitempty"`
}

const (
	cliErrorMessageNotFound    = "message_not_found"
	cliErrorRawMessageNotFound = "raw_message_not_found"
)

var errMutuallyExclusiveScope = errors.New("account and collection are mutually exclusive")

type cliRequestError struct {
	code    string
	message string
}

func (e *cliRequestError) Error() string {
	if e == nil {
		return ""
	}
	return e.message
}

func newCLIRequestError(message string) *cliRequestError {
	return &cliRequestError{code: "invalid_dedup_delete", message: message}
}

type operationErrorPolicy struct {
	InvalidCode    string
	NotFoundCode   string
	NotFoundStatus int
	InternalLog    string
}

var (
	collectionOperationErrorPolicy = operationErrorPolicy{
		InvalidCode:    "invalid_collection",
		NotFoundCode:   "not_found",
		NotFoundStatus: http.StatusNotFound,
		InternalLog:    "failed CLI collection operation",
	}
	identityOperationErrorPolicy = operationErrorPolicy{
		InvalidCode:    "invalid_identity",
		NotFoundCode:   "identity_not_found",
		NotFoundStatus: http.StatusBadRequest,
		InternalLog:    "failed CLI identity operation",
	}
	scopeOperationErrorPolicy = operationErrorPolicy{
		InvalidCode:    "invalid_scope",
		NotFoundCode:   "invalid_scope",
		NotFoundStatus: http.StatusBadRequest,
		InternalLog:    "failed to resolve CLI scope",
	}
	deduplicatePlanOperationErrorPolicy = operationErrorPolicy{
		InvalidCode:    "deduplicate_plan_failed",
		NotFoundCode:   "deduplicate_plan_failed",
		NotFoundStatus: http.StatusBadRequest,
		InternalLog:    "failed CLI deduplicate planning",
	}
	accountOperationErrorPolicy = operationErrorPolicy{
		InvalidCode:    "invalid_account",
		NotFoundCode:   "account_not_found",
		NotFoundStatus: http.StatusNotFound,
		InternalLog:    "failed CLI account operation",
	}
)

type cliCollectionResponse struct {
	ID                 int64                         `json:"id"`
	Name               string                        `json:"name"`
	Description        string                        `json:"description,omitempty"`
	CreatedAt          time.Time                     `json:"created_at"`
	SourceIDs          []int64                       `json:"source_ids"`
	MessageCount       int64                         `json:"message_count"`
	SourceDeletedCount int64                         `json:"source_deleted_count"`
	Sources            []cliCollectionSourceResponse `json:"sources,omitempty"`
}

type cliCollectionSourceResponse struct {
	ID          int64  `json:"id"`
	Identifier  string `json:"identifier"`
	DisplayName string `json:"display_name,omitempty"`
}

type cliIdentityRowResponse struct {
	Account     string     `json:"account"`
	SourceID    int64      `json:"source_id"`
	SourceType  string     `json:"source_type"`
	Identifier  string     `json:"identifier,omitempty"`
	Signals     []string   `json:"signals"`
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
	None        bool       `json:"none,omitempty"`
}

type cliAccountResponse struct {
	ID                 int64      `json:"id"`
	Email              string     `json:"email"`
	Type               string     `json:"type"`
	DisplayName        string     `json:"display_name"`
	OAuthApp           string     `json:"oauth_app,omitempty"`
	MessageCount       int64      `json:"message_count"`
	SourceDeletedCount int64      `json:"source_deleted_count"`
	LastSync           *time.Time `json:"last_sync"`
}

type cliMessageResponse struct {
	ID                   int64                  `json:"id"`
	SourceMessageID      string                 `json:"source_message_id"`
	ConversationID       int64                  `json:"conversation_id"`
	SourceConversationID string                 `json:"source_conversation_id"`
	Subject              string                 `json:"subject"`
	MessageType          string                 `json:"message_type,omitempty"`
	Snippet              string                 `json:"snippet"`
	SentAt               time.Time              `json:"sent_at"`
	ReceivedAt           *time.Time             `json:"received_at"`
	DeletedAt            *time.Time             `json:"deleted_at"`
	SizeEstimate         int64                  `json:"size_estimate"`
	HasAttachments       bool                   `json:"has_attachments"`
	From                 []cliMessageAddress    `json:"from"`
	To                   []cliMessageAddress    `json:"to"`
	Cc                   []cliMessageAddress    `json:"cc"`
	Bcc                  []cliMessageAddress    `json:"bcc"`
	Labels               []string               `json:"labels"`
	Attachments          []cliMessageAttachment `json:"attachments"`
	BodyText             string                 `json:"body_text"`
	BodyHTML             string                 `json:"body_html"`
}

type cliMessageAddress struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

type cliMessageAttachment struct {
	ID          int64  `json:"id"`
	Filename    string `json:"filename"`
	MimeType    string `json:"mime_type"`
	Size        int64  `json:"size"`
	ContentHash string `json:"content_hash"`
	URL         string `json:"url,omitempty"`
}

type cliScope struct {
	Account    collectionops.Scope
	Collection *store.CollectionWithSources
}

func (s cliScope) sourceIDs() []int64 {
	if s.Collection != nil {
		return append([]int64(nil), s.Collection.SourceIDs...)
	}
	return s.Account.SourceIDs()
}

func (s cliScope) displayName() string {
	switch {
	case s.Collection != nil:
		return s.Collection.Name
	case s.Account.Source != nil:
		return s.Account.Source.Identifier
	case len(s.Account.AdditionalSourceIDs) > 0:
		return s.Account.Input
	}
	return ""
}

func (s *Server) handleCLIStats(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "Database not available")
		return
	}

	account := r.URL.Query().Get("account")
	collection := r.URL.Query().Get("collection")
	if account == "" && collection == "" {
		stats, err := s.store.GetStats()
		if err != nil {
			s.logger.Error("failed to get CLI stats", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve statistics")
			return
		}
		writeJSON(w, http.StatusOK, cliStatsResponse{
			Stats: statsResponseFromStore(stats),
		})
		return
	}

	cliStore, apiErr := s.cliStore()
	if apiErr != nil {
		writeAPIHTTPError(w, apiErr)
		return
	}

	scope, err := resolveCLIStatsScope(cliStore, account, collection)
	if err != nil {
		writeAPIHTTPError(w, s.cliScopeError(err))
		return
	}
	sourceIDs := scope.sourceIDs()
	if len(sourceIDs) == 0 {
		writeError(w, http.StatusBadRequest, "empty_scope", cliEmptyScopeMessage(account, collection))
		return
	}

	stats, err := cliStore.GetStatsForScope(sourceIDs)
	if err != nil {
		s.logger.Error("failed to get scoped CLI stats", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve statistics")
		return
	}
	writeJSON(w, http.StatusOK, cliStatsResponse{
		Stats:            statsResponseFromStore(stats),
		ScopeLabel:       scope.displayName(),
		ScopeSourceCount: len(sourceIDs),
	})
}

func (s *Server) handleCLIInitDB(w http.ResponseWriter, _ *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "Database not available")
		return
	}

	migrator, ok := s.store.(CLIStartupMigrationStore)
	if !ok {
		writeAPIHTTPError(w, cliStoreUnavailableError())
		return
	}

	var legacyIdentityAddresses []string
	if s.cfg != nil {
		legacyIdentityAddresses = s.cfg.Identity.Addresses
	}
	migration, err := migrator.RunStartupMigrations(legacyIdentityAddresses)
	if err != nil {
		s.logger.Error("startup migration failed", "error", err)
		writeError(w, http.StatusInternalServerError, "startup_migration_failed", "Startup migrations failed")
		return
	}
	switch {
	case migration.Deferred:
		s.logger.Info("legacy [identity] block in config detected (migration deferred until a source exists)",
			"address_count", migration.AddressCount,
			"hint", "run 'msgvault add-account ...' to create a source; the migration will retry on the next command")
	case migration.Applied:
		s.logger.Info("legacy identity migrated",
			"addresses", migration.AddressCount,
			"sources", migration.SourceCount)
	}

	stats, err := s.store.GetStats()
	if err != nil {
		s.logger.Error("failed to get CLI init-db stats", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve statistics")
		return
	}
	writeJSON(w, http.StatusOK, cliInitDBResponse{
		Stats:  statsResponseFromStore(stats),
		Notice: migration.Notice,
	})
}

func (s *Server) handleCLICacheStats(w http.ResponseWriter, _ *http.Request) {
	if s.cfg == nil {
		writeError(w, http.StatusServiceUnavailable, "config_unavailable", "Config not available")
		return
	}
	stats, err := cacheops.CollectStats(s.cfg.AnalyticsDir())
	if err != nil {
		s.logger.Error("failed to get CLI cache stats", "error", err)
		writeError(w, http.StatusInternalServerError, "cache_stats_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) handleCLIBuildCache(w http.ResponseWriter, r *http.Request) {
	builder, ok := s.store.(CLICacheBuilder)
	if !ok {
		writeAPIHTTPError(w, cliStoreUnavailableError())
		return
	}

	fullRebuild := false
	if raw := r.URL.Query().Get("full_rebuild"); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_full_rebuild", "full_rebuild must be a boolean")
			return
		}
		fullRebuild = parsed
	}

	writeEvent := newCLINDJSONEventWriter[CLICacheBuildEvent](w)
	writeErr := builder.BuildCLICache(r.Context(), fullRebuild, writeEvent)
	if writeErr != nil {
		s.logger.Error("failed to build CLI analytics cache", "error", writeErr)
		if err := writeEvent(CLICacheBuildEvent{Type: cliStreamEventTypeError, Error: writeErr.Error()}); err != nil {
			s.logger.Error("failed to stream CLI build-cache error event", "error", err)
		}
		return
	}
	if err := writeEvent(CLICacheBuildEvent{Type: cliStreamEventTypeComplete}); err != nil {
		s.logger.Error("failed to stream CLI build-cache completion event", "error", err)
	}
}

func (s *Server) handleCLISync(w http.ResponseWriter, r *http.Request) {
	s.handleCLISyncWithMode(w, r, false)
}

func (s *Server) handleCLISyncFull(w http.ResponseWriter, r *http.Request) {
	s.handleCLISyncWithMode(w, r, true)
}

func (s *Server) handleCLISyncWithMode(w http.ResponseWriter, r *http.Request, full bool) {
	runner, ok := s.store.(CLISyncRunner)
	if !ok {
		writeAPIHTTPError(w, cliStoreUnavailableError())
		return
	}
	req, apiErr := parseCLISyncRequest(r, full)
	if apiErr != nil {
		writeAPIHTTPError(w, apiErr)
		return
	}

	writeEvent := newCLINDJSONEventWriter[CLISyncEvent](w)
	if err := runner.RunCLISync(r.Context(), req, writeEvent); err != nil {
		s.logger.Error("failed to run CLI sync", "full", full, "error", err)
		if writeErr := writeEvent(CLISyncEvent{Type: cliStreamEventTypeError, Error: err.Error()}); writeErr != nil {
			s.logger.Error("failed to stream CLI sync error event", "error", writeErr)
		}
		return
	}
	if err := writeEvent(CLISyncEvent{Type: cliStreamEventTypeComplete}); err != nil {
		s.logger.Error("failed to stream CLI sync completion event", "error", err)
	}
}

func parseCLISyncRequest(r *http.Request, full bool) (CLISyncRequest, *apiHTTPError) {
	values := r.URL.Query()
	req := CLISyncRequest{
		Full:   full,
		Email:  values.Get("email"),
		Query:  values.Get("query"),
		Before: values.Get("before"),
		After:  values.Get("after"),
	}
	if rawNoResume := values.Get("noresume"); rawNoResume != "" {
		noResume, err := strconv.ParseBool(rawNoResume)
		if err != nil {
			return CLISyncRequest{}, newAPIHTTPError(
				http.StatusBadRequest,
				"invalid_noresume",
				"noresume must be a boolean",
			)
		}
		req.NoResume = noResume
	}
	if rawLimit := values.Get("limit"); rawLimit != "" {
		limit, err := strconv.Atoi(rawLimit)
		if err != nil || limit < 0 {
			return CLISyncRequest{}, newAPIHTTPError(
				http.StatusBadRequest,
				"invalid_limit",
				"limit must be a non-negative integer",
			)
		}
		req.Limit = limit
	}
	return req, nil
}

func (s *Server) handleCLIVerify(w http.ResponseWriter, r *http.Request) {
	runner, ok := s.store.(CLIVerifyRunner)
	if !ok {
		writeAPIHTTPError(w, cliStoreUnavailableError())
		return
	}
	req, apiErr := parseCLIVerifyRequest(r)
	if apiErr != nil {
		writeAPIHTTPError(w, apiErr)
		return
	}

	writeEvent := newCLINDJSONEventWriter[CLIVerifyEvent](w)
	if err := runner.RunCLIVerify(r.Context(), req, writeEvent); err != nil {
		s.logger.Error("failed to run CLI verify", "email", req.Email, "error", err)
		if writeErr := writeEvent(CLIVerifyEvent{Type: cliStreamEventTypeError, Error: err.Error()}); writeErr != nil {
			s.logger.Error("failed to stream CLI verify error event", "error", writeErr)
		}
		return
	}
	if err := writeEvent(CLIVerifyEvent{Type: cliStreamEventTypeComplete}); err != nil {
		s.logger.Error("failed to stream CLI verify completion event", "error", err)
	}
}

func parseCLIVerifyRequest(r *http.Request) (CLIVerifyRequest, *apiHTTPError) {
	values := r.URL.Query()
	req := CLIVerifyRequest{
		Email:      values.Get("email"),
		SampleSize: 100,
	}
	if req.Email == "" {
		return CLIVerifyRequest{}, newAPIHTTPError(
			http.StatusBadRequest,
			"invalid_email",
			"email is required",
		)
	}
	if rawSample := values.Get("sample"); rawSample != "" {
		sampleSize, err := strconv.Atoi(rawSample)
		if err != nil || sampleSize < 0 {
			return CLIVerifyRequest{}, newAPIHTTPError(
				http.StatusBadRequest,
				"invalid_sample",
				"sample must be a non-negative integer",
			)
		}
		req.SampleSize = sampleSize
	}
	if rawSkipDBCheck := values.Get("skip_db_check"); rawSkipDBCheck != "" {
		skipDBCheck, err := strconv.ParseBool(rawSkipDBCheck)
		if err != nil {
			return CLIVerifyRequest{}, newAPIHTTPError(
				http.StatusBadRequest,
				"invalid_skip_db_check",
				"skip_db_check must be a boolean",
			)
		}
		req.SkipDBCheck = skipDBCheck
	}
	if rawJSON := values.Get("json"); rawJSON != "" {
		jsonOutput, err := strconv.ParseBool(rawJSON)
		if err != nil {
			return CLIVerifyRequest{}, newAPIHTTPError(
				http.StatusBadRequest,
				"invalid_json",
				"json must be a boolean",
			)
		}
		req.JSON = jsonOutput
	}
	return req, nil
}

func (s *Server) handleCLIRepairEncoding(w http.ResponseWriter, r *http.Request) {
	runner, ok := s.store.(CLIRepairEncodingRunner)
	if !ok {
		writeAPIHTTPError(w, cliStoreUnavailableError())
		return
	}

	writeEvent := newCLINDJSONEventWriter[CLIRepairEncodingEvent](w)
	if err := runner.RunCLIRepairEncoding(r.Context(), writeEvent); err != nil {
		s.logger.Error("failed to run CLI repair-encoding", "error", err)
		if writeErr := writeEvent(CLIRepairEncodingEvent{Type: cliStreamEventTypeError, Error: err.Error()}); writeErr != nil {
			s.logger.Error("failed to stream CLI repair-encoding error event", "error", writeErr)
		}
		return
	}
	if err := writeEvent(CLIRepairEncodingEvent{Type: cliStreamEventTypeComplete}); err != nil {
		s.logger.Error("failed to stream CLI repair-encoding completion event", "error", err)
	}
}

func (s *Server) handleCLIRun(w http.ResponseWriter, r *http.Request) {
	runner, ok := s.store.(CLIRunner)
	if !ok {
		writeAPIHTTPError(w, cliStoreUnavailableError())
		return
	}
	var req CLIRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON request body")
		return
	}
	if len(req.Args) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_args", "args must not be empty")
		return
	}
	if !cliRunCommandAllowed(req.Args) {
		writeError(w, http.StatusBadRequest, "command_not_allowed", "command is not allowed through the daemon CLI runner")
		return
	}
	for name := range req.Env {
		if !s.cliRunEnvAllowed(name) {
			writeError(w, http.StatusBadRequest, "env_not_allowed", fmt.Sprintf("env %q is not allowed through the daemon CLI runner", name))
			return
		}
	}

	writeEvent := newCLINDJSONEventWriter[CLIRunEvent](w)
	if err := runner.RunCLICommand(r.Context(), req, writeEvent); err != nil {
		s.logger.Error("failed to run CLI command", "args", req.Args, "error", err)
		if writeErr := writeEvent(CLIRunEvent{Type: cliStreamEventTypeError, Error: err.Error()}); writeErr != nil {
			s.logger.Error("failed to stream CLI run error event", "error", writeErr)
		}
		return
	}
	if err := writeEvent(CLIRunEvent{Type: cliStreamEventTypeComplete}); err != nil {
		s.logger.Error("failed to stream CLI run completion event", "error", err)
	}
}

func (s *Server) handleCLIAddCalendarPlan(w http.ResponseWriter, r *http.Request) {
	planner, ok := s.store.(CLIAddCalendarPlanner)
	if !ok {
		writeAPIHTTPError(w, cliStoreUnavailableError())
		return
	}
	var req CLIAddCalendarPlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON request body")
		return
	}
	resp, err := planner.PlanCLIAddCalendar(r.Context(), req)
	if err != nil {
		writeAPIHTTPError(w, newAPIHTTPError(http.StatusBadRequest, "calendar_plan_failed", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleCLIEmbeddingsPlan(w http.ResponseWriter, r *http.Request) {
	planner, ok := s.store.(CLIEmbeddingsPlanner)
	if !ok {
		writeAPIHTTPError(w, cliStoreUnavailableError())
		return
	}
	var req CLIEmbeddingsPlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON request body")
		return
	}
	resp, err := planner.PlanCLIEmbeddings(r.Context(), req)
	if err != nil {
		writeAPIHTTPError(w, newAPIHTTPError(http.StatusBadRequest, "embeddings_plan_failed", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleCLIDeleteStagedPlan(w http.ResponseWriter, r *http.Request) {
	planner, ok := s.store.(CLIDeleteStagedPlanner)
	if !ok {
		writeAPIHTTPError(w, cliStoreUnavailableError())
		return
	}
	var req CLIDeleteStagedPlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON request body")
		return
	}
	resp, err := planner.PlanCLIDeleteStaged(r.Context(), req)
	if err != nil {
		writeAPIHTTPError(w, newAPIHTTPError(http.StatusBadRequest, "delete_staged_plan_failed", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleCLICreateDeletionManifest(w http.ResponseWriter, r *http.Request) {
	saver, ok := s.store.(CLIDeletionManifestSaver)
	if !ok {
		writeAPIHTTPError(w, cliStoreUnavailableError())
		return
	}
	var manifest deletion.Manifest
	if err := json.NewDecoder(r.Body).Decode(&manifest); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON request body")
		return
	}
	if err := validateCLIDeletionManifest(&manifest); err != nil {
		writeAPIHTTPError(w, err)
		return
	}
	if err := saver.SaveCLIDeletionManifest(r.Context(), &manifest); err != nil {
		s.logger.Error("failed to save CLI deletion manifest", "id", manifest.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "save_deletion_manifest_failed", "Failed to save deletion manifest")
		return
	}
	writeJSON(w, http.StatusOK, CLIDeletionManifestResponse{
		ID:           manifest.ID,
		MessageCount: len(manifest.GmailIDs),
	})
}

func validateCLIDeletionManifest(manifest *deletion.Manifest) *apiHTTPError {
	if manifest == nil {
		return newAPIHTTPError(http.StatusBadRequest, "invalid_manifest", "Deletion manifest is required")
	}
	if strings.TrimSpace(manifest.ID) == "" {
		return newAPIHTTPError(http.StatusBadRequest, "missing_manifest_id", "Deletion manifest ID is required")
	}
	if err := deletion.ValidateManifestID(manifest.ID); err != nil {
		return newAPIHTTPError(http.StatusBadRequest, "invalid_manifest_id", err.Error())
	}
	if len(manifest.GmailIDs) == 0 {
		return newAPIHTTPError(http.StatusBadRequest, "empty_manifest", "Deletion manifest must include at least one Gmail ID")
	}
	if manifest.Status == "" {
		manifest.Status = deletion.StatusPending
	}
	if manifest.Status != deletion.StatusPending {
		return newAPIHTTPError(http.StatusBadRequest, "invalid_manifest_status", "Deletion manifest status must be pending")
	}
	if manifest.Version == 0 {
		manifest.Version = 1
	}
	if manifest.CreatedBy == "" {
		manifest.CreatedBy = "cli"
	}
	return nil
}

// cliRunCommandAllowed reports whether a proxied CLI command may run through
// the daemon CLI runner. Most commands are admitted by their leading word
// alone; command groups whose subcommand matters (currently only "backup")
// are checked against args[1] as well, since e.g. "backup init" and "backup
// verify" run local, unfrozen archive mutations that the daemon's backup
// freeze window does not protect against.
func cliRunCommandAllowed(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] == "backup" {
		return len(args) >= 2 && args[1] == "create"
	}
	switch args[0] {
	case "add-account",
		"add-beeper",
		"add-calendar",
		"add-imap",
		"add-o365",
		"add-synctech-sms-drive",
		"add-teams",
		"backfill-beeper-media",
		"backfill-teams-media",
		"build-embeddings",
		"cancel-deletion",
		"create-subset",
		"deduplicate",
		"delete-staged",
		"embeddings",
		"import",
		"import-emlx",
		"import-gvoice",
		"import-imessage",
		"import-mbox",
		"import-messenger",
		"import-pst",
		"import-synctech-sms",
		"import-whatsapp",
		"list-deletions",
		"logs",
		"pack-attachments",
		"repack-attachments",
		"remove-account",
		"show-deletion",
		"sync-beeper",
		"sync-calendar",
		"sync-synctech-sms",
		"sync-teams":
		return true
	default:
		return false
	}
}

// newCLINDJSONEventWriter streams events as NDJSON. Write deadlines are
// handled once per request by timeoutMiddleware, which clears the server's
// absolute WriteTimeout for long daemon requests; every NDJSON route is in
// isLongDaemonRequest, so the writer itself never touches deadlines.
func newCLINDJSONEventWriter[T any](w http.ResponseWriter) func(T) error {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	flusher, _ := w.(http.Flusher)
	return func(event T) error {
		if err := enc.Encode(event); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
}

// cliRunEnvAllowed permits the static forwarding allowlist plus the
// config-named embedding API key variable, which the frontend CLI forwards
// so a key exported in the caller's shell reaches the embed subprocess.
func (s *Server) cliRunEnvAllowed(name string) bool {
	if clirun.EnvAllowed(name) {
		return true
	}
	if s.cfg == nil {
		return false
	}
	keyEnv := s.cfg.Vector.Embeddings.APIKeyEnv
	return keyEnv != "" && name == keyEnv
}

func (s *Server) cliDedupDeleteStore() (CLIDedupDeleteStore, *apiHTTPError) {
	if s.store == nil {
		return nil, cliDedupStoreUnavailableError()
	}
	dedupStore, ok := s.store.(CLIDedupDeleteStore)
	if !ok {
		return nil, cliDedupStoreUnavailableError()
	}
	return dedupStore, nil
}

func (s *Server) planCLIDeleteDeduped(req cliDeleteDedupedPlanRequest) (cliDeleteDedupedPlanResponse, error) {
	dedupStore, apiErr := s.cliDedupDeleteStore()
	if apiErr != nil {
		return cliDeleteDedupedPlanResponse{}, apiErr
	}

	result, err := planCLIDeleteDedupedWith(dedupStore, req.scope())
	if err != nil {
		return cliDeleteDedupedPlanResponse{}, s.cliDedupDeleteError(err)
	}
	return result, nil
}

func (s *Server) executeCLIDeleteDeduped(req cliDeleteDedupedExecuteRequest) (cliDeleteDedupedExecuteResponse, error) {
	dedupStore, apiErr := s.cliDedupDeleteStore()
	if apiErr != nil {
		return cliDeleteDedupedExecuteResponse{}, apiErr
	}

	plan, err := planCLIDeleteDedupedWith(dedupStore, req.scope())
	if err != nil {
		return cliDeleteDedupedExecuteResponse{}, s.cliDedupDeleteError(err)
	}
	if err := validateCLIDeleteDedupedExpectations(req); err != nil {
		return cliDeleteDedupedExecuteResponse{}, s.cliDedupDeleteError(err)
	}
	if *req.ExpectedTotal != plan.Total {
		return cliDeleteDedupedExecuteResponse{}, newAPIHTTPError(
			http.StatusConflict,
			"dedup_delete_plan_changed",
			"Dedup deletion plan changed; rerun delete-deduped",
		)
	}
	if *req.ExpectedBatchCount != plan.BatchCount {
		return cliDeleteDedupedExecuteResponse{}, newAPIHTTPError(
			http.StatusConflict,
			"dedup_delete_plan_changed",
			"Dedup deletion plan changed; rerun delete-deduped",
		)
	}
	if !req.AllHidden {
		if !dedupExpectedBatchesMatch(req.ExpectedBatches, plan.Batches) {
			return cliDeleteDedupedExecuteResponse{}, dedupDeletePlanChangedError()
		}
	}
	if plan.Total == 0 {
		return cliDeleteDedupedExecuteResponse{BatchCount: plan.BatchCount}, nil
	}

	var backupPath string
	if !req.NoBackup {
		backupPath, err = s.deleteDedupedBackupPath()
		if err != nil {
			return cliDeleteDedupedExecuteResponse{}, newAPIHTTPError(
				http.StatusInternalServerError,
				"dedup_backup_failed",
				fmt.Sprintf("Backup database failed: %v", err),
			)
		}
		if err := dedupStore.BackupDatabase(backupPath); err != nil {
			return cliDeleteDedupedExecuteResponse{}, newAPIHTTPError(
				http.StatusInternalServerError,
				"dedup_backup_failed",
				fmt.Sprintf("Backup database failed: %v", err),
			)
		}
	}

	var deletedTotal int64
	var batchCount int64
	if req.AllHidden {
		deleted, distinct, err := dedupStore.DeleteAllDeduped()
		if err != nil {
			return cliDeleteDedupedExecuteResponse{}, fmt.Errorf("delete all dedup-hidden: %w", err)
		}
		deletedTotal = deleted
		batchCount = distinct
	} else {
		batchCount = int64(len(req.BatchIDs))
		for _, id := range req.BatchIDs {
			deleted, err := dedupStore.DeleteDedupedBatch(id)
			if err != nil {
				return cliDeleteDedupedExecuteResponse{}, fmt.Errorf("delete dedup batch %q: %w", id, err)
			}
			deletedTotal += deleted
		}
	}

	return cliDeleteDedupedExecuteResponse{
		Deleted:    deletedTotal,
		BatchCount: batchCount,
		BackupPath: backupPath,
	}, nil
}

func planCLIDeleteDedupedWith(
	dedupStore CLIDedupDeleteStore,
	req cliDeleteDedupedScopeRequest,
) (cliDeleteDedupedPlanResponse, error) {
	if err := validateCLIDeleteDedupedScope(req); err != nil {
		return cliDeleteDedupedPlanResponse{}, err
	}

	if req.AllHidden {
		total, distinct, err := dedupStore.CountAllDeduped()
		if err != nil {
			return cliDeleteDedupedPlanResponse{}, err
		}
		return cliDeleteDedupedPlanResponse{
			Total:      total,
			BatchCount: distinct,
		}, nil
	}

	stats, total, err := dedupStore.CountDedupedBatches(req.BatchIDs)
	if err != nil {
		return cliDeleteDedupedPlanResponse{}, err
	}
	return cliDeleteDedupedPlanResponse{
		Total:      total,
		BatchCount: int64(len(req.BatchIDs)),
		Batches:    dedupBatchResponses(stats),
	}, nil
}

func validateCLIDeleteDedupedScope(req cliDeleteDedupedScopeRequest) error {
	switch {
	case req.AllHidden && len(req.BatchIDs) > 0:
		return newCLIRequestError("--batch and --all-hidden are mutually exclusive")
	case req.AllHidden:
		return nil
	case len(req.BatchIDs) == 0:
		return newCLIRequestError("must specify --batch or --all-hidden")
	}
	for _, id := range req.BatchIDs {
		if strings.TrimSpace(id) == "" {
			return newCLIRequestError("batch id cannot be empty")
		}
	}
	return nil
}

func validateCLIDeleteDedupedExpectations(req cliDeleteDedupedExecuteRequest) error {
	switch {
	case req.ExpectedTotal == nil:
		return newCLIRequestError("expected_total is required for delete execution")
	case req.ExpectedBatchCount == nil:
		return newCLIRequestError("expected_batch_count is required for delete execution")
	case req.ExpectedBatches == nil:
		return newCLIRequestError("expected_batches is required for delete execution")
	case req.AllHidden && len(req.ExpectedBatches) > 0:
		return newCLIRequestError("expected_batches cannot be used with --all-hidden")
	}
	return nil
}

func dedupExpectedBatchesMatch(
	expected []cliDeleteDedupedBatchResponse,
	actual []cliDeleteDedupedBatchResponse,
) bool {
	if len(expected) != len(actual) {
		return false
	}
	for i := range expected {
		if expected[i].ID != actual[i].ID || expected[i].Count != actual[i].Count {
			return false
		}
	}
	return true
}

func dedupDeletePlanChangedError() *apiHTTPError {
	return newAPIHTTPError(
		http.StatusConflict,
		"dedup_delete_plan_changed",
		"Dedup deletion plan changed; rerun delete-deduped",
	)
}

func dedupBatchResponses(stats []store.DedupedBatchCount) []cliDeleteDedupedBatchResponse {
	out := make([]cliDeleteDedupedBatchResponse, 0, len(stats))
	for _, stat := range stats {
		out = append(out, cliDeleteDedupedBatchResponse{
			ID:    stat.ID,
			Count: stat.Count,
		})
	}
	return out
}

func (s *Server) deleteDedupedBackupPath() (string, error) {
	if s.cfg == nil {
		return "", errors.New("config unavailable")
	}
	dbFilePath, err := s.cfg.DatabasePath()
	if err != nil {
		return "", fmt.Errorf("resolve database path: %w", err)
	}
	return filepath.Join(
		filepath.Dir(dbFilePath),
		filepath.Base(dbFilePath)+".delete-deduped-backup-"+time.Now().Format("20060102-150405"),
	), nil
}

func (s *Server) cliDedupDeleteError(err error) *apiHTTPError {
	if err == nil {
		return nil
	}
	var requestErr *cliRequestError
	if errors.As(err, &requestErr) {
		return newAPIHTTPError(http.StatusBadRequest, requestErr.code, requestErr.message)
	}
	s.logger.Error("failed CLI dedup delete operation", "error", err)
	return newAPIHTTPError(http.StatusInternalServerError, "internal_error", "Dedup delete operation failed")
}

func (s *Server) handleCLISearch(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.engine == nil {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "Database not available")
		return
	}
	cliStore, apiErr := s.cliStore()
	if apiErr != nil {
		writeAPIHTTPError(w, apiErr)
		return
	}

	account := r.URL.Query().Get("account")
	collection := r.URL.Query().Get("collection")

	limit := parseCLISearchInt(r.URL.Query().Get("limit"), 50)
	if limit <= 0 {
		limit = 50
	}
	offset := max(parseCLISearchInt(r.URL.Query().Get("offset"), 0), 0)

	queryStr := r.URL.Query().Get("q")
	parsed := search.Parse(queryStr)
	if err := parsed.Err(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	for _, raw := range r.URL.Query()["message_type"] {
		for typ := range strings.SplitSeq(raw, ",") {
			typ = strings.TrimSpace(strings.ToLower(typ))
			if typ != "" {
				parsed.MessageTypes = append(parsed.MessageTypes, typ)
			}
		}
	}

	var scope cliScope
	if account != "" || collection != "" {
		var err error
		scope, err = resolveCLIStatsScope(cliStore, account, collection)
		if err != nil {
			writeAPIHTTPError(w, s.cliScopeError(err))
			return
		}
		sourceIDs := scope.sourceIDs()
		if len(sourceIDs) == 0 {
			writeError(w, http.StatusBadRequest, "empty_scope", cliEmptyScopeMessage(account, collection))
			return
		}
		parsed.AccountIDs = append(parsed.AccountIDs, sourceIDs...)
	}

	if parsed.IsEmpty() {
		writeError(w, http.StatusBadRequest, "empty_search", "empty search query")
		return
	}

	resp := cliSearchResponse{
		ScopeLabel:       scope.displayName(),
		ScopeSourceCount: len(scope.sourceIDs()),
		// The FTS completeness probe scans every message (a minute on a
		// large archive, once per daemon process), so the search never
		// waits on it: the probe and any backfill run in the background
		// and the response only reports their state so the CLI can warn
		// that results may be incomplete while a backfill runs.
		IndexState: s.ensureCLISearchIndexAsync(cliStore),
	}

	results, err := s.engine.Search(r.Context(), parsed, limit, offset)
	if err != nil {
		s.logger.Error("CLI search failed", "error", err)
		writeError(w, http.StatusInternalServerError, "search_failed", err.Error())
		return
	}
	resp.Results = results
	writeJSON(w, http.StatusOK, resp)
}

// ensureCLISearchIndexAsync makes sure exactly one background worker is
// verifying (and if needed, repopulating) the FTS index, and reports what
// that worker is currently doing so the response can carry it. Searches
// never wait on this work: the completeness probe alone scans every message
// — a minute on a large archive — and it used to run inline on the first
// search of every daemon process, which reads as a hung CLI. A failed
// backfill clears the running flag so a later search retries.
func (s *Server) ensureCLISearchIndexAsync(cliStore CLIStore) string {
	if s.ftsIndexComplete.Load() {
		return ""
	}
	if s.ftsEnsureRunning.CompareAndSwap(false, true) {
		// The quick probe (a couple of index lookups) classifies the initial
		// state: a visibly stale index reports "building" right away, so the
		// very first search already warns that results may be incomplete
		// instead of staying silent for the minutes the full probe can take.
		state := cliSearchIndexStateChecking
		if cliStore.NeedsFTSBackfillQuick() {
			state = cliSearchIndexStateBuilding
		}
		s.ftsIndexState.Store(state)
		go s.runCLISearchIndexEnsure(cliStore)
	}
	state, _ := s.ftsIndexState.Load().(string)
	return state
}

func (s *Server) runCLISearchIndexEnsure(cliStore CLIStore) {
	defer s.ftsEnsureRunning.Store(false)
	// The probe runs outside the operation gate, so a rebuild-fts can clear
	// and repopulate the index while it scans. A "complete" observation is
	// only memoized when no rebuild was in progress at the snapshot (even
	// generation — an odd one means a rebuild handler is mid-flight and the
	// probe's read snapshot may predate its index clear) and none started
	// since; otherwise it is discarded and the next search re-triggers the
	// worker. The backfill path below needs no generation check: it
	// re-probes and backfills under the gate, serialized with rebuilds.
	rebuildGen := s.ftsRebuildGen.Load()
	if !s.probeFTSBackfillNeeded(cliStore) {
		if rebuildGen%2 == 0 && s.ftsRebuildGen.Load() == rebuildGen {
			s.ftsIndexComplete.Store(true)
		}
		s.ftsIndexState.Store("")
		return
	}
	s.ftsIndexState.Store(cliSearchIndexStateBuilding)
	// Background gate work, not request work: a backfill queued behind a
	// long sync should wait its turn rather than force the sync to yield,
	// since no request is blocked on it anymore.
	done, ok := s.beginBackgroundOperationGateWork(context.Background(), "a search index build")
	if !ok {
		s.ftsIndexState.Store("")
		return
	}
	defer done()
	if !cliStore.NeedsFTSBackfill() {
		s.ftsIndexComplete.Store(true)
		s.ftsIndexState.Store("")
		return
	}
	if _, err := s.backfillFTSWithActivity(cliStore); err != nil {
		s.logger.Error("failed to build CLI search index", "error", err)
		s.ftsIndexState.Store("")
		return
	}
	s.ftsIndexComplete.Store(true)
	s.ftsIndexState.Store("")
}

// probeFTSBackfillNeeded runs the expensive first-search completeness probe
// as a labeled activity, and logs the duration when it is slow enough to be
// what a user just sat through.
func (s *Server) probeFTSBackfillNeeded(cliStore CLIStore) bool {
	end := s.beginActivity("checking the search index")
	defer end()
	started := time.Now()
	needs := cliStore.NeedsFTSBackfill()
	if elapsed := time.Since(started); elapsed > time.Second {
		s.logger.Info("search index completeness probe finished",
			"needs_backfill", needs,
			"duration", elapsed.Round(time.Millisecond).String(),
		)
	}
	return needs
}

// backfillFTSWithActivity runs the FTS backfill with its progress mirrored
// into the activity label, so authenticated /health reports live counts
// instead of the gate's static "a search index build".
func (s *Server) backfillFTSWithActivity(cliStore CLIStore) (int64, error) {
	end := s.beginActivity("building the search index")
	defer end()
	started := time.Now()
	s.logger.Info("building CLI search index")
	n, err := cliStore.BackfillFTS(func(done, total int64) {
		s.setActivityLabel(fmt.Sprintf(
			"building the search index (%d/%d messages)", done, total))
	})
	if err != nil {
		return n, err
	}
	s.logger.Info("built CLI search index",
		"indexed", n,
		"duration", time.Since(started).Round(time.Millisecond).String(),
	)
	return n, nil
}

func (s *Server) handleCLIRebuildFTS(w http.ResponseWriter, _ *http.Request) {
	cliStore, apiErr := s.cliStore()
	if apiErr != nil {
		writeAPIHTTPError(w, apiErr)
		return
	}

	writeEvent := newCLINDJSONEventWriter[cliRebuildFTSEvent](w)

	// A rebuild clears and repopulates the FTS index in batches. Invalidate
	// the memoized completeness flag before starting so any concurrent CLI
	// search re-probes (and serializes behind this POST via the operation
	// gate) instead of trusting a stale "complete" cache while the index is
	// mid-rebuild or left incomplete by a failed rebuild. Only a successful
	// rebuild re-sets the flag.
	//
	// The generation is seqlock-style: odd while this handler runs, bumped
	// back to even on return. The ensure worker's ungated probe can overlap
	// a rebuild in either direction — it may start before this bump and
	// finish after (generation moved), or start after the bump while its
	// read snapshot still predates the index clear (generation odd). It
	// only memoizes a "complete" observation on an even, unchanged
	// generation, so both interleavings discard the stale result.
	s.ftsRebuildGen.Add(1)
	defer s.ftsRebuildGen.Add(1)
	s.ftsIndexComplete.Store(false)

	var writeErr error
	indexed, err := cliStore.RebuildFTS(func(done, total int64) {
		if writeErr != nil {
			return
		}
		writeErr = writeEvent(cliRebuildFTSEvent{
			Type:  "progress",
			Done:  done,
			Total: total,
		})
	})
	if err != nil {
		s.logger.Error("failed to rebuild CLI FTS index", "error", err)
		if writeErr == nil {
			writeErr = writeEvent(cliRebuildFTSEvent{
				Type:  "error",
				Error: err.Error(),
			})
		}
		return
	}
	// The rebuild fully repopulated the index; safe to re-memoize so later CLI
	// searches skip the expensive backfill probe again.
	s.ftsIndexComplete.Store(true)
	if writeErr == nil {
		writeErr = writeEvent(cliRebuildFTSEvent{
			Type:    "complete",
			Indexed: indexed,
		})
	}
	if writeErr != nil {
		s.logger.Error("failed to stream CLI FTS rebuild event", "error", writeErr)
	}
}

func (s *Server) handleCLIAccounts(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "Database not available")
		return
	}
	cliStore, apiErr := s.cliStore()
	if apiErr != nil {
		writeAPIHTTPError(w, apiErr)
		return
	}

	sources, err := cliStore.ListSources("")
	if err != nil {
		s.logger.Error("failed to list CLI accounts", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to list accounts")
		return
	}

	accounts := make([]cliAccountResponse, 0, len(sources))
	for _, src := range sources {
		count, err := cliStore.CountMessagesForSource(src.ID)
		if err != nil {
			s.logger.Error("failed to count CLI account messages",
				"source_id", src.ID,
				"identifier", src.Identifier,
				"error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to list accounts")
			return
		}
		sourceDeleted, err := cliStore.CountSourceDeletedMessages(src.ID)
		if err != nil {
			s.logger.Error("failed to count CLI account source-deleted messages",
				"source_id", src.ID,
				"identifier", src.Identifier,
				"error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to list accounts")
			return
		}
		account := cliAccountResponse{
			ID:                 src.ID,
			Email:              src.Identifier,
			Type:               src.SourceType,
			MessageCount:       count,
			SourceDeletedCount: sourceDeleted,
		}
		if src.DisplayName.Valid {
			account.DisplayName = src.DisplayName.String
		}
		if src.OAuthApp.Valid {
			account.OAuthApp = src.OAuthApp.String
		}
		if src.LastSyncAt.Valid {
			lastSync := src.LastSyncAt.Time.UTC()
			account.LastSync = &lastSync
		}
		accounts = append(accounts, account)
	}

	writeJSON(w, http.StatusOK, cliAccountsResponse{Accounts: accounts})
}

func (s *Server) updateCLIAccount(req accountops.UpdateRequest) (accountops.UpdateResult, error) {
	cliStore, apiErr := s.cliStore()
	if apiErr != nil {
		return accountops.UpdateResult{}, apiErr
	}

	result, err := accountops.UpdateDisplayName(cliStore, req)
	if err != nil {
		return accountops.UpdateResult{}, s.operationError(
			err,
			accountOperationErrorPolicy,
			"Failed to update account",
		)
	}
	return result, nil
}

func (s *Server) handleCLICollections(w http.ResponseWriter, r *http.Request) {
	cliStore, apiErr := s.cliStore()
	if apiErr != nil {
		writeAPIHTTPError(w, apiErr)
		return
	}

	collections, err := cliStore.ListCollections()
	if err != nil {
		s.logger.Error("failed to list CLI collections", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to list collections")
		return
	}

	resp := make([]cliCollectionResponse, 0, len(collections))
	for _, coll := range collections {
		item, err := cliCollectionResponseFromStore(cliStore, coll)
		if err != nil {
			s.logger.Error("failed to hydrate CLI collection", "collection", coll.Name, "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to list collections")
			return
		}
		resp = append(resp, item)
	}
	writeJSON(w, http.StatusOK, cliCollectionsResponse{Collections: resp})
}

func (s *Server) handleCLICollection(w http.ResponseWriter, r *http.Request) {
	cliStore, apiErr := s.cliStore()
	if apiErr != nil {
		writeAPIHTTPError(w, apiErr)
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "missing_name", "Collection name is required")
		return
	}
	coll, err := cliStore.GetCollectionByName(name)
	if err != nil {
		if errors.Is(err, store.ErrCollectionNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "Collection not found")
			return
		}
		s.logger.Error("failed to get CLI collection", "name", name, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve collection")
		return
	}
	item, err := cliCollectionResponseFromStore(cliStore, coll)
	if err != nil {
		s.logger.Error("failed to hydrate CLI collection", "collection", coll.Name, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve collection")
		return
	}
	writeJSON(w, http.StatusOK, cliCollectionEnvelope{Collection: item})
}

func (s *Server) createCLICollection(
	req collectionops.CreateRequest,
) (collectionops.MutationResult, error) {
	cliStore, apiErr := s.cliStore()
	if apiErr != nil {
		return collectionops.MutationResult{}, apiErr
	}
	result, err := collectionops.Create(cliStore, req)
	if err != nil {
		return collectionops.MutationResult{}, s.operationError(
			err,
			collectionOperationErrorPolicy,
			"Failed to create collection",
		)
	}
	return result, nil
}

func (s *Server) addCLICollectionSources(
	name string,
	req collectionops.SourcesRequest,
) (collectionops.MutationResult, error) {
	cliStore, apiErr := s.cliStore()
	if apiErr != nil {
		return collectionops.MutationResult{}, apiErr
	}
	result, err := collectionops.AddSources(cliStore, name, req)
	if err != nil {
		return collectionops.MutationResult{}, s.operationError(
			err,
			collectionOperationErrorPolicy,
			"Failed to add collection sources",
		)
	}
	return result, nil
}

func (s *Server) removeCLICollectionSources(
	name string,
	req collectionops.SourcesRequest,
) (collectionops.MutationResult, error) {
	cliStore, apiErr := s.cliStore()
	if apiErr != nil {
		return collectionops.MutationResult{}, apiErr
	}
	result, err := collectionops.RemoveSources(cliStore, name, req)
	if err != nil {
		return collectionops.MutationResult{}, s.operationError(
			err,
			collectionOperationErrorPolicy,
			"Failed to remove collection sources",
		)
	}
	return result, nil
}

func (s *Server) deleteCLICollection(name string) (collectionops.MutationResult, error) {
	cliStore, apiErr := s.cliStore()
	if apiErr != nil {
		return collectionops.MutationResult{}, apiErr
	}
	result, err := collectionops.Delete(cliStore, name)
	if err != nil {
		return collectionops.MutationResult{}, s.operationError(
			err,
			collectionOperationErrorPolicy,
			"Failed to delete collection",
		)
	}
	return result, nil
}

func (s *Server) cliScopeError(err error) *apiHTTPError {
	return s.operationError(err, scopeOperationErrorPolicy, "Failed to resolve CLI scope")
}

func (s *Server) getCLIIdentities(
	account string,
	collection string,
	primaryOnly bool,
) (cliIdentitiesResponse, error) {
	if s.store == nil {
		return cliIdentitiesResponse{}, newAPIHTTPError(
			http.StatusServiceUnavailable,
			"store_unavailable",
			"Database not available",
		)
	}
	cliStore, apiErr := s.cliStore()
	if apiErr != nil {
		return cliIdentitiesResponse{}, apiErr
	}

	if account != "" && collection != "" {
		return cliIdentitiesResponse{}, newAPIHTTPError(
			http.StatusBadRequest,
			"invalid_scope",
			errMutuallyExclusiveScope.Error(),
		)
	}

	var sourceIDs []int64
	switch {
	case primaryOnly:
		if account == "" || collection != "" {
			return cliIdentitiesResponse{}, newAPIHTTPError(
				http.StatusBadRequest,
				"invalid_scope",
				"primary_only requires account scope",
			)
		}
		scope, err := resolveCLIAccountScope(cliStore, account)
		if err != nil {
			return cliIdentitiesResponse{}, s.cliScopeError(err)
		}
		if scope.Account.Source == nil {
			return cliIdentitiesResponse{}, newAPIHTTPError(
				http.StatusBadRequest,
				"invalid_scope",
				fmt.Sprintf("no account found for %q", account),
			)
		}
		sourceIDs = []int64{scope.Account.Source.ID}
	case account != "" || collection != "":
		scope, err := resolveCLIStatsScope(cliStore, account, collection)
		if err != nil {
			return cliIdentitiesResponse{}, s.cliScopeError(err)
		}
		sourceIDs = scope.sourceIDs()
	default:
		sources, err := cliStore.ListSources("")
		if err != nil {
			s.logger.Error("failed to list CLI identity accounts", "error", err)
			return cliIdentitiesResponse{}, newAPIHTTPError(
				http.StatusInternalServerError,
				"internal_error",
				"Failed to list identities",
			)
		}
		sourceIDs = make([]int64, 0, len(sources))
		for _, src := range sources {
			if src != nil {
				sourceIDs = append(sourceIDs, src.ID)
			}
		}
	}

	rows, err := collectCLIIdentityRows(cliStore, sourceIDs)
	if err != nil {
		s.logger.Error("failed to collect CLI identities", "error", err)
		return cliIdentitiesResponse{}, newAPIHTTPError(
			http.StatusInternalServerError,
			"internal_error",
			"Failed to list identities",
		)
	}
	return cliIdentitiesResponse{Rows: rows}, nil
}

func (s *Server) addCLIIdentity(req identityops.AddRequest) (identityops.AddResult, error) {
	cliStore, apiErr := s.cliStore()
	if apiErr != nil {
		return identityops.AddResult{}, apiErr
	}

	result, err := identityops.Add(cliStore, req)
	if err != nil {
		return identityops.AddResult{}, s.operationError(
			err,
			identityOperationErrorPolicy,
			"Failed to add identity",
		)
	}
	return result, nil
}

func (s *Server) removeCLIIdentity(req identityops.RemoveRequest) (identityops.RemoveResult, error) {
	cliStore, apiErr := s.cliStore()
	if apiErr != nil {
		return identityops.RemoveResult{}, apiErr
	}

	result, err := identityops.Remove(cliStore, req)
	if err != nil {
		return identityops.RemoveResult{}, s.operationError(
			err,
			identityOperationErrorPolicy,
			"Failed to remove identity",
		)
	}
	return result, nil
}

func (s *Server) operationError(
	err error,
	policy operationErrorPolicy,
	internalMessage string,
) *apiHTTPError {
	switch opserr.KindOf(err) {
	case opserr.KindInvalid:
		return newAPIHTTPError(http.StatusBadRequest, policy.InvalidCode, err.Error())
	case opserr.KindNotFound:
		return newAPIHTTPError(policy.NotFoundStatus, policy.NotFoundCode, err.Error())
	default:
		s.logger.Error(policy.InternalLog, "error", err)
		return newAPIHTTPError(http.StatusInternalServerError, "internal_error", internalMessage)
	}
}

func (s *Server) handleCLIMessage(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable", "Query engine not available")
		return
	}
	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "missing_id", "Message id is required")
		return
	}

	msg, err := s.resolveCLIMessage(r, idStr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve message")
		return
	}
	if msg == nil {
		writeError(w, http.StatusNotFound, cliErrorMessageNotFound, "Message not found")
		return
	}

	writeJSON(w, http.StatusOK, cliMessageResponseFromQuery(msg))
}

func (s *Server) handleCLIMessageRaw(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable", "Query engine not available")
		return
	}
	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "missing_id", "Message id is required")
		return
	}

	msg, err := s.resolveCLIMessage(r, idStr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve message")
		return
	}
	if msg == nil {
		writeError(w, http.StatusNotFound, cliErrorMessageNotFound, "Message not found")
		return
	}

	raw, err := s.engine.GetMessageRaw(r.Context(), msg.ID)
	if err != nil {
		s.logger.Error("failed to get CLI raw message", "id", msg.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve raw message")
		return
	}
	if raw == nil {
		writeError(w, http.StatusNotFound, cliErrorRawMessageNotFound, "Message raw data not found")
		return
	}

	w.Header().Set("Content-Type", "message/rfc822")
	w.Header().Set("X-Msgvault-Message-Id", strconv.FormatInt(msg.ID, 10))
	w.Header().Set("X-Msgvault-Source-Message-Id", msg.SourceMessageID)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(raw); err != nil {
		s.logger.Error("failed to write CLI raw message", "id", msg.ID, "error", err)
	}
}

func (s *Server) handleCLIAttachment(w http.ResponseWriter, r *http.Request) {
	if s.cfg == nil {
		writeError(w, http.StatusServiceUnavailable, "config_unavailable", "Configuration not available")
		return
	}
	contentHash := r.URL.Query().Get("content_hash")
	if contentHash == "" {
		writeError(w, http.StatusBadRequest, "missing_content_hash", "Content hash is required")
		return
	}
	if err := msgexport.ValidateContentHash(contentHash); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_content_hash", err.Error())
		return
	}

	if s.blobStore != nil {
		rc, size, err := s.blobStore.OpenStream(r.Context(), contentHash)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				writeError(w, http.StatusNotFound, "not_found", "Attachment not found")
				return
			}
			s.logger.Error("failed to open CLI attachment", "content_hash", contentHash, "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve attachment")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.Header().Set("X-Msgvault-Content-Hash", contentHash)
		w.WriteHeader(http.StatusOK)
		_, copyErr := io.Copy(w, rc)
		if err := errors.Join(copyErr, rc.Close()); err != nil {
			s.logger.Error("failed to write CLI attachment", "content_hash", contentHash, "error", err)
		}
		return
	}

	storagePath, err := msgexport.StoragePath(s.cfg.AttachmentsDir(), contentHash)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_content_hash", err.Error())
		return
	}
	// #nosec G304,G703 -- contentHash is validated as a SHA-256 hex digest and
	// StoragePath anchors it under the configured attachments directory.
	//
	// codeql[go/path-injection] -- StoragePath validates the hash and anchors
	// the resulting path under AttachmentsDir.
	f, err := os.Open(storagePath)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "not_found", "Attachment not found")
			return
		}
		s.logger.Error("failed to open CLI attachment", "content_hash", contentHash, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve attachment")
		return
	}
	defer func() { _ = f.Close() }()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Msgvault-Content-Hash", contentHash)
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, f); err != nil {
		s.logger.Error("failed to write CLI attachment", "content_hash", contentHash, "error", err)
	}
}

func (s *Server) resolveCLIMessage(r *http.Request, idStr string) (*query.MessageDetail, error) {
	var (
		msg *query.MessageDetail
		err error
	)
	if id, parseErr := strconv.ParseInt(idStr, 10, 64); parseErr == nil {
		msg, err = s.engine.GetMessage(r.Context(), id)
		if err != nil {
			s.logger.Error("failed to get CLI message by id", "id", id, "error", err)
			return nil, err
		}
	}
	if msg == nil {
		msg, err = s.engine.GetMessageBySourceID(r.Context(), idStr)
		if err != nil {
			s.logger.Error("failed to get CLI message by source id", "id", idStr, "error", err)
			return nil, err
		}
	}
	return msg, nil
}

func cliMessageResponseFromQuery(msg *query.MessageDetail) cliMessageResponse {
	return cliMessageResponse{
		ID:                   msg.ID,
		SourceMessageID:      msg.SourceMessageID,
		ConversationID:       msg.ConversationID,
		SourceConversationID: msg.SourceConversationID,
		Subject:              msg.Subject,
		MessageType:          msg.MessageType,
		Snippet:              msg.Snippet,
		SentAt:               msg.SentAt,
		ReceivedAt:           msg.ReceivedAt,
		DeletedAt:            msg.DeletedAt,
		SizeEstimate:         msg.SizeEstimate,
		HasAttachments:       msg.HasAttachments,
		From:                 cliMessageAddresses(msg.From),
		To:                   cliMessageAddresses(msg.To),
		Cc:                   cliMessageAddresses(msg.Cc),
		Bcc:                  cliMessageAddresses(msg.Bcc),
		Labels:               msg.Labels,
		Attachments:          cliMessageAttachments(msg.Attachments),
		BodyText:             msg.BodyText,
		BodyHTML:             msg.BodyHTML,
	}
}

func cliMessageAddresses(addrs []query.Address) []cliMessageAddress {
	out := make([]cliMessageAddress, len(addrs))
	for i, addr := range addrs {
		out[i] = cliMessageAddress{Email: addr.Email, Name: addr.Name}
	}
	return out
}

func cliMessageAttachments(atts []query.AttachmentInfo) []cliMessageAttachment {
	out := make([]cliMessageAttachment, len(atts))
	for i, att := range atts {
		out[i] = cliMessageAttachment{
			ID:          att.ID,
			Filename:    att.Filename,
			MimeType:    att.MimeType,
			Size:        att.Size,
			ContentHash: att.ContentHash,
			URL:         att.URL,
		}
	}
	return out
}

func cliCollectionResponseFromStore(
	st CLIStore,
	coll *store.CollectionWithSources,
) (cliCollectionResponse, error) {
	if coll == nil {
		return cliCollectionResponse{}, nil
	}
	resp := cliCollectionResponse{
		ID:                 coll.ID,
		Name:               coll.Name,
		Description:        coll.Description,
		CreatedAt:          coll.CreatedAt,
		SourceIDs:          append([]int64(nil), coll.SourceIDs...),
		MessageCount:       coll.MessageCount,
		SourceDeletedCount: coll.SourceDeletedCount,
		Sources:            make([]cliCollectionSourceResponse, 0, len(coll.SourceIDs)),
	}
	for _, sid := range coll.SourceIDs {
		src, err := st.GetSourceByID(sid)
		if err != nil {
			return cliCollectionResponse{}, fmt.Errorf("get source %d: %w", sid, err)
		}
		item := cliCollectionSourceResponse{
			ID:         src.ID,
			Identifier: src.Identifier,
		}
		if src.DisplayName.Valid {
			item.DisplayName = src.DisplayName.String
		}
		resp.Sources = append(resp.Sources, item)
	}
	return resp, nil
}

func collectCLIIdentityRows(st CLIStore, sourceIDs []int64) ([]cliIdentityRowResponse, error) {
	out := make([]cliIdentityRowResponse, 0)
	for _, sid := range sourceIDs {
		src, err := st.GetSourceByID(sid)
		if err != nil {
			return nil, fmt.Errorf("get source %d: %w", sid, err)
		}
		identifiers, err := st.ListAccountIdentities(sid)
		if err != nil {
			return nil, fmt.Errorf("list identities for source %d: %w", sid, err)
		}
		if len(identifiers) == 0 {
			out = append(out, cliIdentityRowResponse{
				Account:    src.Identifier,
				SourceID:   src.ID,
				SourceType: src.SourceType,
				Signals:    []string{},
				None:       true,
			})
			continue
		}
		for _, ai := range identifiers {
			confirmedAt := ai.ConfirmedAt
			signals := identityops.SplitSignalSet(ai.SourceSignal)
			if signals == nil {
				signals = []string{}
			}
			out = append(out, cliIdentityRowResponse{
				Account:     src.Identifier,
				SourceID:    src.ID,
				SourceType:  src.SourceType,
				Identifier:  ai.Address,
				Signals:     signals,
				ConfirmedAt: &confirmedAt,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Identifier < out[j].Identifier
	})
	return out, nil
}

func parseCLISearchInt(raw string, fallback int) int {
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}

func statsResponseFromStore(stats *store.Stats) StatsResponse {
	if stats == nil {
		return StatsResponse{}
	}
	return StatsResponse{
		TotalMessages:         stats.MessageCount,
		ActiveMessages:        stats.MessageCount,
		SourceDeletedMessages: stats.SourceDeletedCount,
		TotalThreads:          stats.ThreadCount,
		TotalAccounts:         stats.SourceCount,
		TotalLabels:           stats.LabelCount,
		TotalAttach:           stats.AttachmentCount,
		DatabaseSize:          stats.DatabaseSize,
	}
}

func resolveCLIStatsScope(st CLIStore, account, collection string) (cliScope, error) {
	switch {
	case account != "" && collection != "":
		return cliScope{}, opserr.Invalid(errMutuallyExclusiveScope)
	case account != "":
		return resolveCLIAccountScope(st, account)
	case collection != "":
		return resolveCLICollectionScope(st, collection)
	default:
		return cliScope{}, nil
	}
}

func cliEmptyScopeMessage(account, collection string) string {
	switch {
	case collection != "":
		return fmt.Sprintf("--collection %q has no member accounts", collection)
	case account != "":
		return fmt.Sprintf("--account %q resolved to zero sources", account)
	default:
		return "scope resolved to zero sources"
	}
}

func resolveCLIAccountScope(st CLIStore, input string) (cliScope, error) {
	scope, err := collectionops.ResolveAccount(st, input)
	return cliScope{Account: scope}, err
}

func resolveCLICollectionScope(st CLIStore, input string) (cliScope, error) {
	collScope, err := collectionops.ResolveCollection(st, input)
	return cliScope{
		Account:    collectionops.Scope{Input: collScope.Input},
		Collection: collScope.Collection,
	}, err
}
