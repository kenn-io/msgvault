package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/vector/visual"
)

const (
	// limitParam names the shared pagination query/form parameter.
	limitParam     = "limit"
	recipientParam = "recipient"
)

const (
	apiKeySecurityScheme = "apiKey"
	cliRouteTag          = "CLI"
)

var configureHumaErrorsOnce sync.Once

type apiHTTPError struct {
	ErrorResponse

	status int
}

func (e *apiHTTPError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return e.ErrorResponse.Error
}

func (e *apiHTTPError) GetStatus() int {
	if e == nil {
		return http.StatusInternalServerError
	}
	return e.status
}

func newAPIHTTPError(status int, code string, message string) *apiHTTPError {
	return &apiHTTPError{
		status: status,
		ErrorResponse: ErrorResponse{
			Error:   code,
			Message: message,
		},
	}
}

func setupHumaErrors() {
	configureHumaErrorsOnce.Do(func() {
		huma.NewError = func(status int, message string, _ ...error) huma.StatusError {
			if message == "" {
				message = http.StatusText(status)
			}
			return newAPIHTTPError(status, errorCodeForStatus(status), message)
		}
	})
}

func errorCodeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusRequestTimeout:
		return "request_timeout"
	case http.StatusUnsupportedMediaType:
		return "unsupported_media_type"
	case http.StatusUnprocessableEntity:
		return "validation_failed"
	case http.StatusPreconditionFailed:
		return "precondition_failed"
	case http.StatusPreconditionRequired:
		return "precondition_required"
	case http.StatusTooManyRequests:
		return "rate_limited"
	case http.StatusServiceUnavailable:
		return "service_unavailable"
	default:
		if status >= 500 {
			return "internal_error"
		}
		return strings.ToLower(strings.ReplaceAll(http.StatusText(status), " ", "_"))
	}
}

func (s *Server) setupHumaAPI(mux humago.Mux) huma.API {
	setupHumaErrors()

	config := huma.DefaultConfig("msgvault API", APISchemaVersion)
	// Disable huma's built-in /docs page: it loads Stoplight Elements from
	// unpkg.com on the same origin as the browser session cookie, so a
	// compromised CDN response could use the session to read archive data or
	// call authenticated mutations. The OpenAPI document at /openapi.json is
	// unaffected.
	config.DocsPath = ""
	// DefaultConfig's only CreateHook installs huma's SchemaLinkTransformer,
	// which injects a `$schema` field (and Link header) into typed
	// huma.Register response bodies. Clearing the hook keeps those routes'
	// success and error bodies on the single bare {error,message} envelope the
	// raw handlers already use, instead of the $schema-wrapped variant.
	config.CreateHooks = nil
	config.Components.Schemas.RegisterTypeAlias(
		reflect.TypeFor[query.MessageSummary](),
		reflect.TypeFor[CLIQueryMessageSummary](),
	)
	if s.daemonVersion != "" {
		if config.Info.Extensions == nil {
			config.Info.Extensions = map[string]any{}
		}
		config.Info.Extensions["x-msgvault-daemon-version"] = s.daemonVersion
	}
	config.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		apiKeySecurityScheme: {
			Type: "apiKey",
			In:   headerParamLocation,
			Name: "X-Api-Key",
		},
	}

	return humago.New(mux, config)
}

func (s *Server) setupAPIV1Group(api huma.API) huma.API {
	apiV1 := huma.NewGroup(api, "/api/v1")
	apiV1.UseMiddleware(s.humaAuthMiddleware)
	return apiV1
}

func withAPIKeySecurity(op huma.Operation) huma.Operation {
	op.Security = []map[string][]string{
		{apiKeySecurityScheme: {}},
	}
	return op
}

func (s *Server) humaAuthMiddleware(ctx huma.Context, next func(huma.Context)) {
	req, _ := humago.Unwrap(ctx)
	if s.requestAuthentication(req).Mode != AuthModeRequired {
		next(ctx)
		return
	}

	s.logUnauthorizedAPIRequest(req)
	writeHumaError(ctx, http.StatusUnauthorized, "unauthorized", "Invalid or missing API key")
}

func writeHumaError(ctx huma.Context, status int, code string, message string) {
	ctx.SetHeader("Content-Type", applicationJSONMediaType)
	ctx.SetStatus(status)
	_ = json.NewEncoder(ctx.BodyWriter()).Encode(ErrorResponse{ //nolint:errchkjson // best-effort error response write
		Error:   code,
		Message: message,
	})
}

func (s *Server) registerHumaRoutes(api huma.API, apiV1 huma.API) {
	s.registerSessionRoutes(api)
	registerRawHumaJSONRoute[HealthResponse](api, huma.Operation{
		OperationID: "health",
		Method:      http.MethodGet,
		Path:        "/health",
		Tags:        []string{"System"},
		Summary:     "Health check",
	}, s.handleHealth)
	registerRawHumaRoute(api, huma.Operation{
		OperationID: "headHealth",
		Method:      http.MethodHead,
		Path:        "/health",
		Tags:        []string{"System"},
		Summary:     "Health check",
	}, s.handleHealth)
	registerAPIV1RawHumaJSONRoute[HealthResponse](apiV1, "getHealth", http.MethodGet, "/health", "Get authenticated health details", s.handleAuthenticatedHealth)
	registerRawHumaJSONRoute[daemon.PingInfo](api, huma.Operation{
		OperationID: "daemonPing",
		Method:      http.MethodGet,
		Path:        daemon.DefaultPingPath,
		Tags:        []string{"Daemon"},
		Summary:     "Daemon discovery ping",
	}, daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: "msgvault",
		Version: s.daemonVersion,
	}).ServeHTTP)
	registerRawHumaRoute(api, huma.Operation{
		OperationID: "daemonIdentity",
		Method:      http.MethodGet,
		Path:        DaemonIdentityPath,
		Tags:        []string{"Daemon"},
		Summary:     "Prove local daemon identity",
		Hidden:      true,
		Responses:   rawHumaResponses(http.StatusNoContent),
	}, s.handleDaemonIdentity)
	registerRawHumaRoute(api, huma.Operation{
		OperationID: "daemonShutdown",
		Method:      http.MethodPost,
		Path:        DaemonShutdownPath,
		Tags:        []string{"Daemon"},
		Summary:     "Stop the local daemon",
		Hidden:      true,
		Responses:   rawHumaResponses(http.StatusAccepted),
	}, s.handleDaemonShutdown)

	registerAPIV1RawHumaJSONRoute[StatsResponse](apiV1, "getStats", http.MethodGet, "/stats", "Get archive statistics", s.handleStats)
	s.registerSettingsRoutes(apiV1)
	s.registerCardDAVRoutes(apiV1)
	s.registerSavedViewRoutes(apiV1)
	s.registerExploreRoutes(apiV1)
	s.registerFilesRoutes(apiV1)
	s.registerDocumentSearchRoute(apiV1)
	s.registerPersonProfileRoutes(apiV1)
	s.registerPersonTrackingRoutes(apiV1)
	s.registerPersonMergeRoutes(apiV1)
	s.registerOrganizationRoutes(apiV1)
	s.registerEmploymentRoutes(apiV1)
	s.registerActivityRoutes(apiV1)
	s.registerPersonProfileValueRoutes(apiV1)
	s.registerCommunicationServiceRoutes(apiV1)
	s.registerAttributeDefinitionRoutes(apiV1)
	s.registerPersonAttributeRoutes(apiV1)
	s.registerParticipantRoutes(apiV1)
	s.registerRelationshipRoutes(apiV1)
	s.registerPersonRelationshipRoutes(apiV1)
	s.registerIdentityLinkRoutes(apiV1)
	s.registerIdentityMatchRoutes(apiV1)
	s.registerTaskIntegrationRoutes(apiV1)
	s.registerTaskLinkRoutes(apiV1)
	s.registerSearchCoverageRoute(apiV1)
	visualSearchOp := rawAPIV1Operation("searchVisualAttachments", http.MethodPost, "/search/attachments/visual", "Search visual attachment content")
	// Tagged Exploration: the handler reads the committed vector index and
	// calls the embedding provider without touching archive state, so the
	// operation gate classifies it with the other analytical POST reads.
	visualSearchOp.Tags = []string{"Exploration"}
	visualSearchOp.RequestBody = jsonRequestBodyFor[visualTextSearchRequest](apiV1)
	// Image queries arrive as multipart/form-data with an `image` file field
	// plus the same scalar fields as the JSON body.
	visualSearchOp.RequestBody.Content["multipart/form-data"] = &huma.MediaType{
		Schema: &huma.Schema{
			Type: huma.TypeObject,
			Properties: map[string]*huma.Schema{
				"image":    {Type: huma.TypeString, Format: "binary", Description: "Query image (JPEG, PNG, WebP, or still GIF)"},
				limitParam: {Type: huma.TypeString}, "sender_person_id": {Type: huma.TypeString},
				"person_id": {Type: huma.TypeString}, "participant_id": {Type: huma.TypeString},
				"direction": {Type: huma.TypeArray, Items: &huma.Schema{Type: huma.TypeString, Enum: []any{"from_person", "to_person", "group"}}},
				"source_id": {Type: huma.TypeString}, "message_id": {Type: huma.TypeString},
				"filename": {Type: huma.TypeString}, "mime_prefix": {Type: huma.TypeString},
				"cursor": {Type: huma.TypeString}, "after": {Type: huma.TypeString}, "before": {Type: huma.TypeString},
			},
			Required: []string{"image"},
		},
	}
	visualSearchOp.Responses = jsonResponsesFor[visual.SearchResponse](apiV1)
	addErrorResponses(apiV1, visualSearchOp.Responses, http.StatusBadRequest, http.StatusConflict,
		http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(apiV1, visualSearchOp, s.handleVisualSearch)
	registerAPIV1RawHumaJSONRoute[visual.Status](apiV1, "getVisualAttachmentStatus", http.MethodGet, "/multimodal/status", "Get visual attachment embedding status", s.handleVisualStatus)
	registerAPIV1RawHumaJSONRouteWithRequest[visualBuildRequest, visual.Status](apiV1, "startVisualAttachmentBuild", http.MethodPost, "/multimodal/build", "Consent and run one bounded visual attachment embedding pass", s.handleVisualBuild)
	registerAPIV1RawHumaJSONRoute[visual.Status](apiV1, "resumeVisualAttachmentBuild", http.MethodPost, "/multimodal/run", "Resume one bounded visual attachment embedding pass", s.handleVisualRun)
	registerAPIV1RawHumaJSONRouteWithRequest[visualRetryRequest, visual.Status](apiV1, "retryVisualAttachmentOwner", http.MethodPost, "/multimodal/retry", "Retry one visual attachment owner", s.handleVisualRetry)
	retireVisualOp := withAPIKeySecurity(huma.Operation{OperationID: "retireVisualAttachmentGeneration", Method: http.MethodPost, Path: "/multimodal/retire", Tags: []string{"Search"}, Summary: "Retire the visual attachment generation", Responses: rawHumaResponses(http.StatusNoContent)})
	retireVisualOp.RequestBody = jsonRequestBodyFor[visualRetireRequest](apiV1)
	registerRawHumaRoute(apiV1, retireVisualOp, s.handleVisualRetire)
	registerAPIV1RawHumaJSONRoute[cliInitDBResponse](apiV1, "initCLIArchive", http.MethodPost, "/cli/init-db", "Initialize the archive for CLI use", s.handleCLIInitDB)
	registerAPIV1RawHumaJSONRoute[cliStatsResponse](apiV1, "getCLIStats", http.MethodGet, "/cli/stats", "Get CLI-compatible archive statistics", s.handleCLIStats)
	registerAPIV1RawHumaJSONRoute[cliSearchResponse](apiV1, "searchCLI", http.MethodGet, "/cli/search", "Search messages for CLI output", s.handleCLISearch)
	registerAPIV1RawHumaJSONRoute[cliAccountsResponse](apiV1, "listCLIAccounts", http.MethodGet, "/cli/accounts", "List accounts for CLI output", s.handleCLIAccounts)
	registerAPIV1RawHumaJSONRoute[cliCacheStatsResponse](apiV1, "getCLICacheStats", http.MethodGet, "/cli/cache-stats", "Get CLI-compatible analytics cache statistics", s.handleCLICacheStats)
	registerAPIV1RawHumaNDJSONRoute[CLICacheBuildEvent](apiV1, "buildCLICache", http.MethodPost, "/cli/build-cache", "Build the CLI analytics cache", s.handleCLIBuildCache)
	registerAPIV1RawHumaNDJSONRoute[CLISyncEvent](apiV1, "syncCLI", http.MethodPost, "/cli/sync", "Run CLI incremental sync", s.handleCLISync)
	registerAPIV1RawHumaNDJSONRoute[CLISyncEvent](apiV1, "syncFullCLI", http.MethodPost, "/cli/sync-full", "Run CLI full sync", s.handleCLISyncFull)
	registerAPIV1RawHumaNDJSONRoute[CLIVerifyEvent](apiV1, "verifyCLI", http.MethodPost, "/cli/verify", "Verify the CLI archive against Gmail", s.handleCLIVerify)
	registerAPIV1RawHumaNDJSONRoute[CLIRepairEncodingEvent](apiV1, "repairEncodingCLI", http.MethodPost, "/cli/repair-encoding", "Repair CLI archive encoding", s.handleCLIRepairEncoding)
	registerAPIV1RawHumaJSONRouteWithRequest[CLIAddCalendarPlanRequest, CLIAddCalendarPlanResponse](apiV1, "planCLIAddCalendar", http.MethodPost, "/cli/add-calendar/plan", "Plan CLI Calendar account setup", s.handleCLIAddCalendarPlan)
	registerAPIV1RawHumaJSONRouteWithRequest[CLIDeleteStagedPlanRequest, CLIDeleteStagedPlanResponse](apiV1, "planCLIDeleteStaged", http.MethodPost, "/cli/delete-staged/plan", "Plan CLI staged deletion execution", s.handleCLIDeleteStagedPlan)
	registerAPIV1RawHumaJSONRouteWithRequest[deletion.Manifest, CLIDeletionManifestResponse](apiV1, "createCLIDeletionManifest", http.MethodPost, "/cli/deletion-manifests", "Create a staged deletion manifest", s.handleCLICreateDeletionManifest)
	registerAPIV1RawHumaJSONRouteWithRequest[CLIEmbeddingsPlanRequest, CLIEmbeddingsPlanResponse](apiV1, "planCLIEmbeddings", http.MethodPost, "/cli/embeddings/plan", "Plan CLI embeddings management", s.handleCLIEmbeddingsPlan)
	registerAPIV1RawHumaNDJSONRouteWithRequest[CLIRunRequest, CLIRunEvent](apiV1, "runCLI", http.MethodPost, "/cli/run", "Run an allowlisted CLI command", s.handleCLIRun)
	registerAPIV1RawHumaJSONRoute[cliMessageResponse](apiV1, "getCLIMessage", http.MethodGet, "/cli/message", "Get one message for CLI output", s.handleCLIMessage)
	registerAPIV1RawHumaBinaryRoute(
		apiV1,
		"getCLIMessageRaw",
		http.MethodGet,
		"/cli/message/raw",
		"Get one raw message for CLI export",
		"message/rfc822",
		s.handleCLIMessageRaw,
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusNotFound,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	)
	registerAPIV1RawHumaBinaryRoute(
		apiV1,
		"getCLIAttachment",
		http.MethodGet,
		"/cli/attachment",
		"Get one attachment for CLI export",
		"application/octet-stream",
		s.handleCLIAttachment,
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusNotFound,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	)
	registerAPIV1RawHumaJSONRoute[cliCollectionsResponse](apiV1, "listCLICollections", http.MethodGet, "/cli/collections", "List collections for CLI output", s.handleCLICollections)
	registerAPIV1RawHumaJSONRoute[cliCollectionEnvelope](apiV1, "getCLICollection", http.MethodGet, "/cli/collection", "Get one collection for CLI output", s.handleCLICollection)
	s.registerCLIAccountHumaRoutes(apiV1)
	s.registerCLICollectionHumaRoutes(apiV1)
	s.registerCLIIdentityHumaRoutes(apiV1)
	s.registerCLIIdentityDiscoveryRoute(apiV1)
	s.registerCLIDedupHumaRoutes(apiV1)
	registerAPIV1RawHumaNDJSONRoute[cliRebuildFTSEvent](apiV1, "rebuildCLIFTS", http.MethodPost, "/cli/rebuild-fts", "Rebuild the CLI full-text search index", s.handleCLIRebuildFTS)

	registerAPIV1RawHumaJSONRoute[MessageListResponse](apiV1, "listMessages", http.MethodGet, "/messages", "List messages", s.handleListMessages)
	registerAPIV1RawHumaJSONRoute[MessageDetail](apiV1, "getMessage", http.MethodGet, "/messages/{id}", "Get one message", s.handleGetMessage)
	s.registerMeetingImportRoute(apiV1)
	registerAPIV1RawHumaJSONRoute[ConversationResponse](apiV1, "getConversation", http.MethodGet, "/conversations/{id}", "Get a bounded containing conversation", s.handleGetConversation)
	registerAPIV1RawHumaJSONRoute[AttachmentInfo](apiV1, "getAttachment", http.MethodGet, "/attachments/{id}", "Get attachment metadata", s.handleGetAttachment)
	registerAPIV1RawHumaBinaryRoute(
		apiV1,
		"getFileContent",
		http.MethodGet,
		"/files/{id}/content",
		"Download content for one authoritative file row",
		"*/*",
		s.handleGetFileContent,
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusNotFound,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	)
	registerAPIV1RawHumaBinaryRoute(
		apiV1,
		"getAttachmentContent",
		http.MethodGet,
		"/attachments/{hash}/content",
		"Download attachment content by SHA-256 hash",
		"*/*",
		s.handleGetAttachmentContent,
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusNotFound,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	)
	registerAPIV1RawHumaBinaryRoute(
		apiV1,
		"getMessageInlinePart",
		http.MethodGet,
		"/messages/{id}/inline",
		"Get an inline MIME part",
		"application/octet-stream",
		s.handleMessageInline,
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusNotFound,
		http.StatusUnsupportedMediaType,
		http.StatusInternalServerError,
		http.StatusNotImplemented,
		http.StatusServiceUnavailable,
	)
	// POST (not GET) so browsers treat the proxy as an unsafe method: the
	// session CSRF middleware then requires same-origin plus X-Csrf-Token,
	// and an <img> embed can never trigger an authenticated outbound fetch.
	registerAPIV1RawHumaBinaryRouteWithRequest[RemoteImageRequest](
		apiV1,
		"getRemoteImage",
		http.MethodPost,
		"/content/remote-image",
		"Fetch a consented remote mail image through the SSRF-hardened daemon proxy",
		"image/*",
		s.handleRemoteImage,
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusUnsupportedMediaType,
		http.StatusBadGateway,
	)
	registerAPIV1RawHumaJSONOneOfRoute(apiV1, "searchMessages", http.MethodGet, "/search", "Search messages", s.handleSearch, reflect.TypeFor[SearchResult](), reflect.TypeFor[hybridSearchResponse]())

	registerAPIV1RawHumaJSONRouteWithRequest[QueryRequest, query.QueryResult](apiV1, "runQuery", http.MethodPost, "/query", "Run an aggregate query", s.handleQuery)
	registerAPIV1RawHumaJSONRoute[AggregateResponse](apiV1, "getAggregates", http.MethodGet, "/aggregates", "Get aggregate rows", s.handleAggregates)
	registerAPIV1RawHumaJSONRoute[AggregateResponse](apiV1, "getSubAggregates", http.MethodGet, "/aggregates/sub", "Get nested aggregate rows", s.handleSubAggregates)
	registerAPIV1RawHumaJSONRoute[FilteredMessagesResponse](apiV1, "filterMessages", http.MethodGet, "/messages/filter", "List filtered messages", s.handleFilteredMessages)
	// The change feed's error statuses are contract, not incident: a 400 means
	// a cursor the consumer must abandon, while 401, 429, 500 and 503 are
	// conditions to retry from the same cursor. Declaring them keeps that
	// distinction in the generated client rather than in every consumer's
	// hand-written decode.
	//
	// 429 is on the list because it is not the handler's to raise and is
	// reached anyway: every request passes this endpoint's dedicated limiter,
	// including trusted loopback traffic, because its whole usage pattern is
	// polling. Undeclared, the generated
	// client reports it as an unexpected status and drops the body a consumer
	// needs to tell "slow down" from "give up on this cursor".
	registerAPIV1RawHumaJSONRouteWithErrors[ChangesResponse](
		apiV1, "listChangedMessages", http.MethodGet, "/messages/changes",
		"List messages whose content changed since a cursor", s.changeFeedGuard(s.handleMessageChanges),
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	)
	registerAPIV1RawHumaJSONRoute[GmailIDsResponse](apiV1, "getGmailIDsByFilter", http.MethodGet, "/messages/gmail-ids", "List Gmail message IDs matching a filter", s.handleGmailIDsByFilter)
	registerAPIV1RawHumaJSONRoute[TotalStatsResponse](apiV1, "getTotalStats", http.MethodGet, "/stats/total", "Get aggregate totals", s.handleTotalStats)
	registerAPIV1RawHumaJSONRoute[FilteredMessagesResponse](apiV1, "searchMessagesByDomains", http.MethodGet, "/search/domains", "Search messages by participant domains", s.handleSearchByDomains)
	registerAPIV1RawHumaJSONRoute[similarSearchResponse](apiV1, "findSimilarMessages", http.MethodGet, "/search/similar", "Find messages similar to a seed message", s.handleSimilarSearch)
	registerAPIV1RawHumaJSONRoute[SearchFastResponse](apiV1, "fastSearch", http.MethodGet, "/search/fast", "Run fast aggregate search", s.handleFastSearch)
	registerAPIV1RawHumaJSONRoute[DeepSearchResponse](apiV1, "deepSearch", http.MethodGet, "/search/deep", "Run full-text message search", s.handleDeepSearch)
	registerAPIV1RawHumaJSONRoute[TextConversationsResponse](apiV1, "listTextConversations", http.MethodGet, "/text/conversations", "List text conversations", s.handleTextConversations)
	registerAPIV1RawHumaJSONRoute[AggregateResponse](apiV1, "getTextAggregates", http.MethodGet, "/text/aggregates", "Get text aggregate rows", s.handleTextAggregates)
	registerAPIV1RawHumaJSONRoute[TextMessagesResponse](apiV1, "listTextConversationMessages", http.MethodGet, "/text/conversations/{id}/messages", "List messages in a text conversation", s.handleTextConversationMessages)
	registerAPIV1RawHumaJSONRoute[TextSearchResponse](apiV1, "searchTextMessages", http.MethodGet, "/text/search", "Search text messages", s.handleTextSearch)
	registerAPIV1RawHumaJSONRoute[TotalStatsResponse](apiV1, "getTextStats", http.MethodGet, "/text/stats", "Get text message totals", s.handleTextStats)

	registerAPIV1RawHumaJSONRoute[AccountListResponse](apiV1, "listAccounts", http.MethodGet, "/accounts", "List scheduler-configured accounts (with sync schedules); use /cli/accounts for all archived sources", s.handleListAccounts)
	registerAPIV1RawHumaJSONRouteWithRequest[AddAccountRequest, StatusMessageResponse](apiV1, "addAccount", http.MethodPost, "/accounts", "Add an account", s.handleAddAccount, http.StatusOK, http.StatusCreated)
	registerAPIV1RawHumaJSONRoute[SourceStatusResponse](apiV1, "listSourceStatus", http.MethodGet, "/sources/status", "List source sync status", s.handleSourceStatus)
	registerAPIV1RawHumaJSONRoute[SourceIdentitiesResponse](apiV1, "listSourceIdentities", http.MethodGet, "/sources/{source_id}/identities", "List confirmed identities for one source", s.handleSourceIdentities)
	registerAPIV1RawHumaJSONRoute[StatusMessageResponse](apiV1, "triggerSync", http.MethodPost, "/sync/{account}", "Trigger account sync", s.handleTriggerSync, http.StatusAccepted)
	registerAPIV1RawHumaJSONRoute[SchedulerStatusResponse](apiV1, "getSchedulerStatus", http.MethodGet, "/scheduler/status", "Get scheduler status", s.handleSchedulerStatus)
	registerAPIV1RawHumaJSONRouteWithRequest[TokenUploadRequest, StatusMessageResponse](apiV1, "uploadToken", http.MethodPost, "/auth/token/{email}", "Upload an OAuth token", s.handleUploadToken, http.StatusCreated)

	registerAPIV1RawHumaJSONRoute[backupFreezeBeginResponse](apiV1, "beginBackupFreeze", http.MethodPost, "/backup/freeze/begin", "Begin a backup freeze window", s.handleBackupFreezeBegin)
	registerAPIV1RawHumaJSONRouteWithRequest[backupFreezeEndRequest, backupFreezeEndResponse](apiV1, "endBackupFreeze", http.MethodPost, "/backup/freeze/end", "End a backup freeze window", s.handleBackupFreezeEnd)

	registerAPIV1RawHumaJSONRouteWithRequest[StageDeletionRequest, StageDeletionResponse](
		apiV1, "stageDeletion", http.MethodPost, "/deletions",
		"Stage messages for deletion", s.handleStageDeletion,
		http.StatusOK, http.StatusCreated)
	registerAPIV1RawHumaJSONRoute[ListDeletionsResponse](
		apiV1, "listDeletions", http.MethodGet, "/deletions",
		"List staged deletion manifests", s.handleListDeletions)
	registerAPIV1RawHumaJSONRoute[DeletionManifestDetail](
		apiV1, "getDeletion", http.MethodGet, "/deletions/{id}",
		"Inspect a staged deletion manifest", s.handleGetDeletion)
	registerAPIV1RawHumaJSONRoute[CancelDeletionResponse](
		apiV1, "cancelDeletion", http.MethodDelete, "/deletions/{id}",
		"Cancel a staged deletion manifest", s.handleCancelDeletion)
}

func registerAPIV1RawHumaJSONRoute[T any](
	api huma.API,
	operationID string,
	method string,
	path string,
	summary string,
	handler http.HandlerFunc,
	successStatuses ...int,
) {
	op := rawAPIV1Operation(operationID, method, path, summary)
	op.Responses = jsonResponsesFor[T](api, successStatuses...)
	registerRawHumaRoute(api, op, handler)
}

// registerAPIV1RawHumaJSONRouteWithErrors is registerAPIV1RawHumaJSONRoute for
// an endpoint whose error statuses are part of its published contract rather
// than incidental. Declaring them makes the generated client model each one, so
// a consumer can branch on the `error` code in the body instead of decoding it
// by hand off an opaque status; the catch-all `default` response stays, so a
// status not listed here still decodes as an error.
func registerAPIV1RawHumaJSONRouteWithErrors[T any](
	api huma.API,
	operationID string,
	method string,
	path string,
	summary string,
	handler http.HandlerFunc,
	errorStatuses ...int,
) {
	op := rawAPIV1Operation(operationID, method, path, summary)
	op.Responses = jsonResponsesFor[T](api)
	for _, status := range errorStatuses {
		op.Responses[httpStatusKey(status)] = errorResponseFor(api)
	}
	registerRawHumaRoute(api, op, handler)
}

func registerAPIV1RawHumaJSONRouteWithRequest[Req any, Resp any](
	api huma.API,
	operationID string,
	method string,
	path string,
	summary string,
	handler http.HandlerFunc,
	successStatuses ...int,
) {
	op := rawAPIV1Operation(operationID, method, path, summary)
	op.RequestBody = jsonRequestBodyFor[Req](api)
	op.Responses = jsonResponsesFor[Resp](api, successStatuses...)
	registerRawHumaRoute(api, op, handler)
}

func registerAPIV1RawHumaJSONOneOfRoute(
	api huma.API,
	operationID string,
	method string,
	path string,
	summary string,
	handler http.HandlerFunc,
	responseTypes ...reflect.Type,
) {
	op := rawAPIV1Operation(operationID, method, path, summary)
	op.Responses = oneOfJSONResponses(api, responseTypes...)
	registerRawHumaRoute(api, op, handler)
}

//nolint:unparam // Retain method for symmetry with the other raw-route helpers and future non-GET binary endpoints.
func registerAPIV1RawHumaBinaryRoute(
	api huma.API,
	operationID string,
	method string,
	path string,
	summary string,
	contentType string,
	handler http.HandlerFunc,
	errorStatuses ...int,
) {
	op := rawAPIV1Operation(operationID, method, path, summary)
	op.Responses = binaryResponsesFor(api, contentType, errorStatuses...)
	registerRawHumaRoute(api, op, handler)
}

func registerAPIV1RawHumaBinaryRouteWithRequest[Req any](
	api huma.API,
	operationID string,
	method string,
	path string,
	summary string,
	contentType string,
	handler http.HandlerFunc,
	errorStatuses ...int,
) {
	op := rawAPIV1Operation(operationID, method, path, summary)
	op.RequestBody = jsonRequestBodyFor[Req](api)
	op.Responses = binaryResponsesFor(api, contentType, errorStatuses...)
	registerRawHumaRoute(api, op, handler)
}

func registerAPIV1RawHumaNDJSONRoute[T any](
	api huma.API,
	operationID string,
	method string,
	path string,
	summary string,
	handler http.HandlerFunc,
) {
	op := rawAPIV1Operation(operationID, method, path, summary)
	op.Responses = ndjsonResponsesFor[T](api)
	registerRawHumaRoute(api, op, handler)
}

func registerAPIV1RawHumaNDJSONRouteWithRequest[Req any, Resp any](
	api huma.API,
	operationID string,
	method string,
	path string,
	summary string,
	handler http.HandlerFunc,
) {
	op := rawAPIV1Operation(operationID, method, path, summary)
	op.RequestBody = jsonRequestBodyFor[Req](api)
	op.Responses = ndjsonResponsesFor[Resp](api)
	registerRawHumaRoute(api, op, handler)
}

func registerRawHumaJSONRoute[T any](api huma.API, op huma.Operation, handler http.HandlerFunc) {
	op.Responses = jsonResponsesFor[T](api)
	registerRawHumaRoute(api, op, handler)
}

func rawAPIV1Operation(operationID, method, path, summary string) huma.Operation {
	return withAPIKeySecurity(huma.Operation{
		OperationID: operationID,
		Method:      method,
		Path:        path,
		Tags:        []string{"API"},
		Summary:     summary,
		Errors:      []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusInternalServerError},
		Parameters:  rawRouteParameters(operationID),
	})
}

func rawRouteParameters(operationID string) []*huma.Param {
	switch operationID {
	case "getCLIStats":
		return scopeParams()
	case "searchCLI":
		return append([]*huma.Param{
			queryStringParam("q", "Search query", true),
			queryIntegerParam(limitParam, "Maximum number of rows to return"),
			queryIntegerParam("offset", "Zero-based row offset"),
			queryStringParam("message_type", "Message type filter; repeat or comma-separate for multiple values", false),
		}, scopeParams()...)
	case "searchDocuments":
		return []*huma.Param{
			queryStringParam("q", "Extracted document content or filename query", true),
			queryIntegerArrayParam("source_id", "Source IDs to include; repeat or comma-separate values"),
			queryRefArrayParam("message_type", "Message types to include; repeat or comma-separate values"),
			queryIntegerParam("attachment_id", "Exact attachment occurrence ID"),
			queryIntegerParam("message_id", "Exact containing message ID"),
			queryIntegerParam("person_id", "Durable person ID"),
			queryIntegerParam("participant_id", "Observed participant ID; translated through its durable person when bound"),
			queryRefArrayParam("direction", "Person relation; repeat or comma-separate from_person, to_person, or group"),
			queryStringParam("after", "Only messages on or after an RFC3339 or YYYY-MM-DD date", false),
			queryStringParam("before", "Only messages before an RFC3339 or YYYY-MM-DD date", false),
			queryIntegerParam(limitParam, "Maximum results to return (default 20, max 100)"),
			queryStringParam("cursor", "Opaque cursor from the previous document search page", false),
		}
	case "getDocumentIndexStatus":
		mediaTypes := queryRefArrayParam("media_type", "Allowed document media types")
		mediaTypes.Required = true
		return []*huma.Param{
			queryStringParam("profile_id", "Exact document extraction profile ID", true),
			queryStringParam("input_key", "Exact extraction input key", true),
			mediaTypes,
			queryRefArrayParam("message_type", "Allowed message types"),
		}
	case "getCLIMessage", "getCLIMessageRaw":
		return []*huma.Param{queryStringParam("id", "Message numeric ID or source message ID", true)}
	case "getCLIAttachment":
		return []*huma.Param{queryStringParam("content_hash", "Attachment SHA-256 content hash", true)}
	case "getCLICollection":
		return []*huma.Param{queryStringParam("name", "Collection name", true)}
	case "buildCLICache":
		return []*huma.Param{queryBooleanParam("full_rebuild", "Rebuild all cache files from scratch")}
	case "syncCLI":
		return []*huma.Param{
			queryStringParam("email", "Account email or display name to sync", false),
			queryIntegerParam("source_id", "Exact source ID to sync"),
			queryRefArrayParam("folder", "IMAP folder names to include (repeatable)"),
			queryRefArrayParam("skip-folder", "IMAP folder names to exclude (repeatable)"),
		}
	case "syncFullCLI":
		return []*huma.Param{
			queryStringParam("email", "Account email or display name to sync", false),
			queryIntegerParam("source_id", "Exact source ID to sync"),
			queryStringParam("query", "Gmail search query", false),
			queryStringParam("after", "Only messages on or after this YYYY-MM-DD date", false),
			queryStringParam("before", "Only messages before this YYYY-MM-DD date", false),
			queryIntegerParam(limitParam, "Maximum messages to sync"),
			queryBooleanParam("noresume", "Ignore checkpoints and start fresh"),
			queryRefArrayParam("folder", "IMAP folder names to include (repeatable)"),
			queryRefArrayParam("skip-folder", "IMAP folder names to exclude (repeatable)"),
		}
	case "verifyCLI":
		return []*huma.Param{
			queryStringParam("email", "Account email to verify", true),
			queryIntegerParam("sample", "Number of messages to sample for MIME verification"),
			queryBooleanParam("skip_db_check", "Skip SQLite integrity check"),
			queryBooleanParam("json", "Emit JSON summary output"),
		}
	case "listMessages":
		return paginationParams("page", "page_size")
	case "getMessage":
		return []*huma.Param{pathIntegerParam("Message ID")}
	case "listMessageTasks", "createOrLinkMessageTask":
		params := []*huma.Param{pathIntegerParam("Archived email message ID")}
		if operationID == "createOrLinkMessageTask" {
			params = append(params, param("X-Request-Id", headerParamLocation, "string", "Browser-generated retry-stable request ID", true))
		}
		return params
	case "listIdentityMatchCandidates":
		return []*huma.Param{
			queryStringParam("state",
				"Candidate state filter (candidate, accepted, rejected, conflict); "+
					"repeat or comma-separate for multiple values", false),
			queryIntegerParam(limitParam, "Maximum candidates to return (default 100, max 500)"),
			queryIntegerParam("offset", "Zero-based candidate offset"),
		}
	case "acceptIdentityMatchCandidate", "rejectIdentityMatchCandidate":
		return []*huma.Param{pathIntegerParam("Identity match candidate ID")}
	case "searchIntegrationTasks":
		return []*huma.Param{queryStringParam("q", "Task title search within the configured project", true)}
	case "unlinkMessageTask":
		return []*huma.Param{
			pathIntegerParam("Archived email message ID"),
			pathStringParam("task_id", "External task ID"),
		}
	case "getConversation":
		return []*huma.Param{
			pathIntegerParam("Conversation ID"),
			queryRequiredIntegerParam("anchor", "Selected message ID anchoring the chronological window"),
			queryIntegerParam("before", "Messages before the anchor (default 25, max 50)"),
			queryIntegerParam("after", "Messages after the anchor (default 25, max 50)"),
			queryStringParam("start",
				"Lower UTC bound, inclusive (RFC3339). Restricts the window, before/after "+
					"counts, and has_before/has_after to messages in [start, end)", false),
			queryStringParam("end",
				"Upper UTC bound, exclusive (RFC3339). Restricts the window, before/after "+
					"counts, and has_before/has_after to messages in [start, end)", false),
		}
	case "listDeletions":
		return []*huma.Param{queryStringParam("status",
			"Filter manifests by status (pending, in_progress, completed, failed, cancelled)", false)}
	case "getDeletion", "cancelDeletion":
		return []*huma.Param{pathStringParam("id", "Deletion manifest ID")}
	case "getAttachment":
		return []*huma.Param{pathIntegerParam("Attachment ID")}
	case "getFile", "getFileContent":
		return []*huma.Param{pathIntegerParam("File attachment ID")}
	case "getParticipant", "getParticipantTimeline", "getParticipantContextSummary", "searchParticipantFiles", "listParticipantInboxes":
		return []*huma.Param{pathIntegerParam("Observed participant cluster member ID")}
	case "getRelationshipTimeline", "getRelationshipCalendar":
		return []*huma.Param{pathIntegerParam("Any member participant ID of the counterpart's identity cluster")}
	case "getDomain", "getDomainTimeline", "getDomainContextSummary", "searchDomainFiles":
		return []*huma.Param{pathStringParam("domain", "Exact normalized domain fact")}
	case "getAttachmentContent":
		return []*huma.Param{pathStringParam("hash", "Attachment SHA-256 content hash")}
	case "getMessageInlinePart":
		return []*huma.Param{
			pathIntegerParam("Message ID"),
			queryStringParam("cid", "Inline MIME Content-ID", true),
		}
	case "searchMessages":
		return mergeParams([]*huma.Param{
			queryStringParam("q", "Search query", true),
			queryStringParam("mode", "Search mode: fts, vector, or hybrid. Structured filter parameters are supported only in vector and hybrid modes", false),
			queryIntegerParam("page", "One-based page number (default 1; values below 1 are clamped to 1). Non-numeric values are rejected with 400."),
			queryIntegerParam("page_size", "Page size (default 20, max 100; out-of-range values are clamped). Non-numeric values are rejected with 400."),
			queryIntegerParam("offset", "Zero-based ranking offset for vector or hybrid search (default 0)"),
			queryBooleanParam("explain", "Include score explanation when mode is vector or hybrid"),
			queryBooleanParam("include_matches", "Include scored semantic chunk excerpts for vector or hybrid results"),
			queryNumberParam("min_score", "Minimum chunk score for included excerpts; does not filter ranked messages"),
			queryStringParam("message_type", "Message type filter; repeat or comma-separate for multiple values", false),
		}, scopeParams(), semanticMessageFilterParams())
	case "getAggregates":
		return append([]*huma.Param{
			queryStringParam("view_type", "Aggregate view type", false),
		}, aggregateOptionParams()...)
	case "getSubAggregates":
		// Aggregate params first so the sort/limit docs reflect
		// parseAggregateOptions (the handler's actual source for those
		// values) rather than the message-filter sort enum, which the
		// sub-aggregate endpoint does not accept.
		return append([]*huma.Param{
			queryStringParam("view_type", "Aggregate view type", true),
		}, mergeParams(aggregateOptionParams(), messageFilterParams())...)
	case "filterMessages":
		return messageFilterParams()
	case "listChangedMessages":
		return changesParams()
	case "getGmailIDsByFilter":
		return messageFilterParams()
	case "searchMessagesByDomains":
		return []*huma.Param{
			queryStringParam("domains", "Comma-separated participant domains", true),
			queryStringParam("after", "Lower date/time bound (RFC3339 or YYYY-MM-DD)", false),
			queryStringParam("before", "Upper date/time bound (RFC3339 or YYYY-MM-DD)", false),
			queryIntegerParam("offset", "Zero-based row offset"),
			queryIntegerParam(limitParam, "Maximum number of rows to return"),
		}
	case "findSimilarMessages":
		return []*huma.Param{
			queryRequiredIntegerParam("message_id", "Seed message ID"),
			queryIntegerParam(limitParam, "Maximum number of rows to return"),
			queryStringParam("account", "Account email or configured source identifier", false),
			queryStringParam("message_type", "Message type filter", false),
			queryStringParam("after", "Lower date/time bound (RFC3339 or YYYY-MM-DD)", false),
			queryStringParam("before", "Upper date/time bound (RFC3339 or YYYY-MM-DD)", false),
			queryBooleanParam("has_attachment", "Only include messages with attachments"),
		}
	case "getTotalStats":
		return []*huma.Param{
			queryIntegerParam("source_id", "Source ID"),
			queryIntegerArrayParam("source_ids", "Source IDs; repeat the parameter for multiple sources"),
			queryBooleanParam("attachments_only", "Only include messages with attachments"),
			queryBooleanParam("hide_deleted", "Exclude deleted messages"),
			queryStringParam("search_query", "Search query", false),
			queryBooleanParam("search_scope", "Include all message types when the search has no explicit message_type"),
			queryStringParam("group_by", "Aggregate view type for grouping", false),
		}
	case "fastSearch":
		params := append([]*huma.Param{
			queryStringParam("q", "Search query", true),
			queryStringParam("view_type", "Stats grouping view type", false),
		}, messageFilterParams()...)
		return append(params,
			queryIntegerArrayParam("source_ids", "Source IDs; repeat the parameter for multiple sources"),
		)
	case "deepSearch":
		return append([]*huma.Param{
			queryStringParam("q", "Search query", true),
			queryStringParam("scope", "Exact search scope: body; omit for composite full-text search", false),
		}, messageFilterParams()...)
	case "listTextConversations":
		return textFilterParams()
	case "getTextAggregates":
		return []*huma.Param{
			queryStringParam("view_type", "Text aggregate view type", false),
			queryStringParam("sort", "Sort field: count or name", false),
			queryStringParam("direction", "Sort direction: asc or desc", false),
			queryIntegerParam(limitParam, "Maximum number of rows to return"),
			queryStringParam("time_granularity", "Time bucket granularity", false),
			queryIntegerParam("source_id", "Source ID"),
			queryStringParam("search_query", "Search query", false),
			queryStringParam("after", "Lower date/time bound (RFC3339 or YYYY-MM-DD)", false),
			queryStringParam("before", "Upper date/time bound (RFC3339 or YYYY-MM-DD)", false),
		}
	case "listTextConversationMessages":
		return append([]*huma.Param{
			pathIntegerParam("Conversation ID"),
			queryStringParam("search_query", "Full-text search within the conversation", false),
		}, textFilterParams()...)
	case "searchTextMessages":
		return []*huma.Param{
			queryStringParam("q", "Search query", true),
			queryIntegerParam("offset", "Zero-based row offset"),
			queryIntegerParam(limitParam, "Maximum number of rows to return"),
		}
	case "getTextStats":
		return []*huma.Param{
			queryIntegerParam("source_id", "Source ID"),
			queryStringParam("search_query", "Search query", false),
		}
	case "listSourceStatus":
		return []*huma.Param{queryStringParam("source_type", "Restrict to one source type", false)}
	case "listSourceIdentities":
		return []*huma.Param{pathNamedIntegerParam("source_id", "Source ID")}
	case "triggerSync":
		return []*huma.Param{
			pathStringParam("account", "Account email or configured source identifier"),
			queryStringParam("source_type", "Source type; required to trigger a generic (non-account) source", false),
		}
	case "uploadToken":
		return []*huma.Param{pathStringParam("email", "Account email address")}
	default:
		return nil
	}
}

func scopeParams() []*huma.Param {
	return []*huma.Param{
		queryStringParam("account", "Restrict to one account/source", false),
		queryStringParam("collection", "Restrict to one collection", false),
	}
}

func paginationParams(pageName, pageSizeName string) []*huma.Param {
	return []*huma.Param{
		queryIntegerParam(pageName, "One-based page number (default 1; values below 1 are clamped to 1). Non-numeric values are rejected with 400."),
		queryIntegerParam(pageSizeName, "Page size (default 20, max 100; out-of-range values are clamped). Non-numeric values are rejected with 400."),
	}
}

func aggregateOptionParams() []*huma.Param {
	return []*huma.Param{
		queryStringParam("sort", "Sort field: count, size, attachment_size, or name", false),
		queryStringParam("direction", "Sort direction: asc or desc", false),
		queryIntegerParam(limitParam, "Maximum number of rows to return (default 100; values below 1 fall back to the default)"),
		queryStringParam("time_granularity", "Time bucket granularity", false),
		queryIntegerParam("source_id", "Source ID"),
		queryBooleanParam("attachments_only", "Only include messages with attachments"),
		queryBooleanParam("hide_deleted", "Exclude deleted messages"),
		queryStringParam("search_query", "Search query", false),
		queryStringParam("after", "Lower date/time bound (RFC3339 or YYYY-MM-DD)", false),
		queryStringParam("before", "Upper date/time bound (RFC3339 or YYYY-MM-DD)", false),
	}
}

func messageFilterParams() []*huma.Param {
	return []*huma.Param{
		queryStringParam("sender", "Sender email/address filter", false),
		queryStringParam("sender_name", "Sender display-name filter", false),
		queryStringParam(recipientParam, "Recipient email/address filter", false),
		queryStringParam("recipient_name", "Recipient display-name filter", false),
		queryStringParam("domain", "Domain filter", false),
		queryStringParam("label", "Label filter", false),
		queryStringParam("message_type", "Message type filter", false),
		queryStringParam("time_period", "Named time period", false),
		queryStringParam("time_granularity", "Time bucket granularity", false),
		queryIntegerParam("conversation_id", "Conversation ID"),
		queryIntegerParam("source_id", "Source ID"),
		queryBooleanParam("attachments_only", "Only include messages with attachments"),
		queryBooleanParam("hide_deleted", "Exclude deleted messages"),
		queryStringParam("after", "Lower date/time bound (RFC3339 or YYYY-MM-DD)", false),
		queryStringParam("before", "Upper date/time bound (RFC3339 or YYYY-MM-DD)", false),
		queryStringParam("empty_targets", "Comma-separated aggregate view names to match empty values", false),
		queryIntegerParam("offset", "Zero-based row offset"),
		queryIntegerParam(limitParam, "Maximum number of rows to return (default and max 500; larger values are clamped)"),
		queryStringParam("sort", "Sort field: date, size, or subject", false),
		queryStringParam("direction", "Sort direction: asc or desc", false),
	}
}

func semanticMessageFilterParams() []*huma.Param {
	return []*huma.Param{
		queryStringParam("sender", "Exact sender email/address filter (vector or hybrid mode only)", false),
		queryStringParam(recipientParam, "Exact recipient email filter across to, cc, and bcc (vector or hybrid mode only)", false),
		queryStringParam("domain", "Exact sender domain filter (vector or hybrid mode only)", false),
		queryStringParam("label", "Exact case-insensitive label filter (vector or hybrid mode only)", false),
		queryStringParam("time_period", "Calendar period in YYYY, YYYY-MM, or YYYY-MM-DD format (vector or hybrid mode only)", false),
		queryStringParam("time_granularity", "Time bucket granularity (vector or hybrid mode only)", false),
		queryIntegerParam("source_id", "Exact source ID (vector or hybrid mode only)"),
		queryBooleanParam("attachments_only", "Only include messages with attachments (vector or hybrid mode only)"),
		queryStringParam("after", "Lower date/time bound (RFC3339 or YYYY-MM-DD; vector or hybrid mode only)", false),
		queryStringParam("before", "Upper date/time bound (RFC3339 or YYYY-MM-DD; vector or hybrid mode only)", false),
	}
}

// changesParams documents the content-change feed's cursor. It is opaque so
// that what the feed tracks can change without breaking consumers: nothing
// outside this server is allowed to depend on its contents.
func changesParams() []*huma.Param {
	return []*huma.Param{
		queryStringParam("cursor",
			"Opaque cursor from the next_cursor of the previous response, sent back "+
				"verbatim. Do not parse, construct, compare, or order it; its contents may "+
				"change without notice. Omit, or send it empty, to start from the beginning "+
				"of the archive. The token is not authenticated: the server does not sign "+
				"it and cannot tell one it issued from a well-formed one you built, so a "+
				"fabricated cursor naming this archive is accepted and simply moves your "+
				"own position. Rejected with 400 invalid_cursor, rather than read as the "+
				"beginning: a token the server cannot read, one carrying a cursor format "+
				"this build does not speak, and one issued against a different archive", false),
		// No published minimum/maximum: the handler clamps rather than rejects,
		// so a range in the schema would make a generated client refuse
		// requests the server answers with 200 and would contradict this
		// description. Every other clamping limit in this API is unbounded in
		// the schema for the same reason.
		queryIntegerParam(limitParam,
			"Maximum number of rows to return (default 100, max 500; values below 1 fall back to the default)"),
	}
}

func textFilterParams() []*huma.Param {
	return []*huma.Param{
		queryIntegerParam("source_id", "Source ID"),
		queryIntegerArrayParam("participant_id", "Exact participant cluster member IDs"),
		queryStringParam("contact_phone", "Sender phone/address filter", false),
		queryStringParam("contact_name", "Sender display-name filter", false),
		queryStringParam("source_type", "Source type filter", false),
		queryStringParam("label", "Label filter", false),
		queryStringParam("time_period", "Named time period", false),
		queryStringParam("time_granularity", "Time bucket granularity", false),
		queryStringParam("after", "Lower date/time bound (RFC3339 or YYYY-MM-DD)", false),
		queryStringParam("before", "Upper date/time bound (RFC3339 or YYYY-MM-DD)", false),
		queryIntegerParam("offset", "Zero-based row offset"),
		queryIntegerParam(limitParam, "Maximum number of rows to return"),
		queryStringParam("sort", "Sort field: last_message, count, or name", false),
		queryStringParam("direction", "Sort direction: asc or desc", false),
	}
}

func mergeParams(groups ...[]*huma.Param) []*huma.Param {
	seen := map[string]struct{}{}
	merged := []*huma.Param{}
	for _, group := range groups {
		for _, p := range group {
			key := p.In + "\x00" + p.Name
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			merged = append(merged, p)
		}
	}
	return merged
}

func pathStringParam(name, doc string) *huma.Param {
	return param(name, pathKey, huma.TypeString, doc, true)
}

func pathIntegerParam(doc string) *huma.Param {
	return pathNamedIntegerParam("id", doc)
}

func pathNamedIntegerParam(name, doc string) *huma.Param {
	p := param(name, pathKey, huma.TypeInteger, doc, true)
	p.Schema.Format = formatInt64
	return p
}

func queryStringParam(name, doc string, required bool) *huma.Param {
	return param(name, "query", huma.TypeString, doc, required)
}

func queryIntegerParam(name, doc string) *huma.Param {
	p := param(name, "query", huma.TypeInteger, doc, false)
	p.Schema.Format = formatInt64
	return p
}

func queryIntegerArrayParam(name, doc string) *huma.Param {
	p := param(name, "query", huma.TypeArray, doc, false)
	p.Schema.Items = &huma.Schema{Type: huma.TypeInteger, Format: formatInt64}
	return p
}

func queryRefArrayParam(name, doc string) *huma.Param {
	p := param(name, "query", huma.TypeArray, doc, false)
	p.Schema.Items = &huma.Schema{Type: huma.TypeString}
	return p
}

func queryRequiredIntegerParam(name, doc string) *huma.Param {
	p := queryIntegerParam(name, doc)
	p.Required = true
	return p
}

func queryBooleanParam(name, doc string) *huma.Param {
	return param(name, "query", huma.TypeBoolean, doc, false)
}

func queryNumberParam(name, doc string) *huma.Param {
	return param(name, "query", huma.TypeNumber, doc, false)
}

func param(name, in, typ, doc string, required bool) *huma.Param {
	return &huma.Param{
		Name:        name,
		In:          in,
		Description: doc,
		Required:    required,
		Schema:      &huma.Schema{Type: typ},
	}
}

func jsonRequestBodyFor[T any](api huma.API) *huma.RequestBody {
	return &huma.RequestBody{
		Required: true,
		Content: map[string]*huma.MediaType{
			applicationJSONMediaType: {Schema: schemaFor[T](api)},
		},
	}
}

func jsonResponsesFor[T any](api huma.API, successStatuses ...int) map[string]*huma.Response {
	if len(successStatuses) == 0 {
		successStatuses = []int{http.StatusOK}
	}
	responses := make(map[string]*huma.Response, len(successStatuses)+1)
	for _, status := range successStatuses {
		responses[httpStatusKey(status)] = &huma.Response{
			Description: http.StatusText(status),
			Content: map[string]*huma.MediaType{
				applicationJSONMediaType: {Schema: schemaFor[T](api)},
			},
		}
	}
	responses["default"] = errorResponseFor(api)
	return responses
}

func oneOfJSONResponses(api huma.API, responseTypes ...reflect.Type) map[string]*huma.Response {
	oneOf := make([]*huma.Schema, 0, len(responseTypes))
	for _, typ := range responseTypes {
		oneOf = append(oneOf, api.OpenAPI().Components.Schemas.Schema(typ, true, ""))
	}
	return map[string]*huma.Response{
		httpStatusKey(http.StatusOK): {
			Description: http.StatusText(http.StatusOK),
			Content: map[string]*huma.MediaType{
				applicationJSONMediaType: {Schema: &huma.Schema{OneOf: oneOf}},
			},
		},
		"default": errorResponseFor(api),
	}
}

func binaryResponsesFor(api huma.API, contentType string, errorStatuses ...int) map[string]*huma.Response {
	responses := map[string]*huma.Response{
		httpStatusKey(http.StatusOK): {
			Description: http.StatusText(http.StatusOK),
			Content: map[string]*huma.MediaType{
				contentType: {Schema: &huma.Schema{Type: huma.TypeString, Format: "binary"}},
			},
		},
		"default": errorResponseFor(api),
	}
	for _, status := range errorStatuses {
		responses[httpStatusKey(status)] = errorResponseFor(api)
	}
	return responses
}

func ndjsonResponsesFor[T any](api huma.API) map[string]*huma.Response {
	return map[string]*huma.Response{
		httpStatusKey(http.StatusOK): {
			Description: http.StatusText(http.StatusOK),
			Content: map[string]*huma.MediaType{
				"application/x-ndjson": {Schema: schemaFor[T](api)},
			},
		},
		"default": errorResponseFor(api),
	}
}

func errorResponseFor(api huma.API) *huma.Response {
	return &huma.Response{
		Description: "Error",
		Content: map[string]*huma.MediaType{
			applicationJSONMediaType: {Schema: schemaFor[ErrorResponse](api)},
		},
	}
}

func schemaFor[T any](api huma.API) *huma.Schema {
	return api.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[T](), true, "")
}

func registerRawHumaRoute(api huma.API, op huma.Operation, handler http.HandlerFunc) {
	if operationDeclaresJSONRequestBody(&op) {
		handler = enforceJSONRequestMediaType(handler)
	}
	if op.Responses == nil {
		status := http.StatusOK
		if op.Method == http.MethodHead {
			status = http.StatusOK
		}
		op.Responses = rawHumaResponses(status)
	}

	if documenter, ok := api.(huma.OperationDocumenter); ok {
		documenter.DocumentOperation(&op)
	} else if !op.Hidden {
		api.OpenAPI().AddOperation(&op)
	}

	handlerWithMiddleware := api.Middlewares().Handler(op.Middlewares.Handler(func(ctx huma.Context) {
		req, w := humago.Unwrap(ctx)
		handler(w, req)
	}))
	api.Adapter().Handle(&op, handlerWithMiddleware)
}

func rawHumaResponses(successStatuses ...int) map[string]*huma.Response {
	responses := make(map[string]*huma.Response, len(successStatuses)+1)
	for _, status := range successStatuses {
		responses[httpStatusKey(status)] = &huma.Response{Description: http.StatusText(status)}
	}
	responses["default"] = &huma.Response{Description: "Error"}
	return responses
}

func httpStatusKey(status int) string {
	return strconv.Itoa(status)
}
