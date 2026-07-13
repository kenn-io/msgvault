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
			In:   "header",
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
	if s.apiRequestAuthorized(req) {
		next(ctx)
		return
	}

	s.logUnauthorizedAPIRequest(req)
	writeHumaError(ctx, http.StatusUnauthorized, "unauthorized", "Invalid or missing API key")
}

func writeHumaError(ctx huma.Context, status int, code string, message string) {
	ctx.SetHeader("Content-Type", "application/json")
	ctx.SetStatus(status)
	_ = json.NewEncoder(ctx.BodyWriter()).Encode(ErrorResponse{ //nolint:errchkjson // best-effort error response write
		Error:   code,
		Message: message,
	})
}

func (s *Server) registerHumaRoutes(api huma.API, apiV1 huma.API) {
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
		OperationID: "daemonShutdown",
		Method:      http.MethodPost,
		Path:        DaemonShutdownPath,
		Tags:        []string{"Daemon"},
		Summary:     "Stop the local daemon",
		Hidden:      true,
		Responses:   rawHumaResponses(http.StatusAccepted),
	}, s.handleDaemonShutdown)

	registerAPIV1RawHumaJSONRoute[StatsResponse](apiV1, "getStats", http.MethodGet, "/stats", "Get archive statistics", s.handleStats)
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
	s.registerCLIDedupHumaRoutes(apiV1)
	registerAPIV1RawHumaNDJSONRoute[cliRebuildFTSEvent](apiV1, "rebuildCLIFTS", http.MethodPost, "/cli/rebuild-fts", "Rebuild the CLI full-text search index", s.handleCLIRebuildFTS)

	registerAPIV1RawHumaJSONRoute[MessageListResponse](apiV1, "listMessages", http.MethodGet, "/messages", "List messages", s.handleListMessages)
	registerAPIV1RawHumaJSONRoute[MessageDetail](apiV1, "getMessage", http.MethodGet, "/messages/{id}", "Get one message", s.handleGetMessage)
	registerAPIV1RawHumaJSONRoute[AttachmentInfo](apiV1, "getAttachment", http.MethodGet, "/attachments/{id}", "Get attachment metadata", s.handleGetAttachment)
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
	registerAPIV1RawHumaJSONOneOfRoute(apiV1, "searchMessages", http.MethodGet, "/search", "Search messages", s.handleSearch, reflect.TypeFor[SearchResult](), reflect.TypeFor[hybridSearchResponse]())

	registerAPIV1RawHumaJSONRouteWithRequest[QueryRequest, query.QueryResult](apiV1, "runQuery", http.MethodPost, "/query", "Run an aggregate query", s.handleQuery)
	registerAPIV1RawHumaJSONRoute[AggregateResponse](apiV1, "getAggregates", http.MethodGet, "/aggregates", "Get aggregate rows", s.handleAggregates)
	registerAPIV1RawHumaJSONRoute[AggregateResponse](apiV1, "getSubAggregates", http.MethodGet, "/aggregates/sub", "Get nested aggregate rows", s.handleSubAggregates)
	registerAPIV1RawHumaJSONRoute[FilteredMessagesResponse](apiV1, "filterMessages", http.MethodGet, "/messages/filter", "List filtered messages", s.handleFilteredMessages)
	registerAPIV1RawHumaJSONRoute[GmailIDsResponse](apiV1, "getGmailIDsByFilter", http.MethodGet, "/messages/gmail-ids", "List Gmail message IDs matching a filter", s.handleGmailIDsByFilter)
	registerAPIV1RawHumaJSONRoute[TotalStatsResponse](apiV1, "getTotalStats", http.MethodGet, "/stats/total", "Get aggregate totals", s.handleTotalStats)
	registerAPIV1RawHumaJSONRoute[FilteredMessagesResponse](apiV1, "searchMessagesByDomains", http.MethodGet, "/search/domains", "Search messages by participant domains", s.handleSearchByDomains)
	registerAPIV1RawHumaJSONRoute[similarSearchResponse](apiV1, "findSimilarMessages", http.MethodGet, "/search/similar", "Find messages similar to a seed message", s.handleSimilarSearch)
	registerAPIV1RawHumaJSONRoute[SearchFastResponse](apiV1, "fastSearch", http.MethodGet, "/search/fast", "Run fast aggregate search", s.handleFastSearch)
	registerAPIV1RawHumaJSONRoute[DeepSearchResponse](apiV1, "deepSearch", http.MethodGet, "/search/deep", "Run full-text message search", s.handleDeepSearch)
	registerAPIV1RawHumaJSONRoute[TextConversationsResponse](apiV1, "listTextConversations", http.MethodGet, "/text/conversations", "List text conversations", s.handleTextConversations)
	registerAPIV1RawHumaJSONRoute[AggregateResponse](apiV1, "getTextAggregates", http.MethodGet, "/text/aggregates", "Get text aggregate rows", s.handleTextAggregates)
	registerAPIV1RawHumaJSONRoute[TextMessagesResponse](apiV1, "listTextConversationMessages", http.MethodGet, "/text/conversations/{id}/messages", "List messages in a text conversation", s.handleTextConversationMessages)
	registerAPIV1RawHumaJSONRoute[TextMessagesResponse](apiV1, "searchTextMessages", http.MethodGet, "/text/search", "Search text messages", s.handleTextSearch)
	registerAPIV1RawHumaJSONRoute[TotalStatsResponse](apiV1, "getTextStats", http.MethodGet, "/text/stats", "Get text message totals", s.handleTextStats)

	registerAPIV1RawHumaJSONRoute[AccountListResponse](apiV1, "listAccounts", http.MethodGet, "/accounts", "List scheduler-configured accounts (with sync schedules); use /cli/accounts for all archived sources", s.handleListAccounts)
	registerAPIV1RawHumaJSONRouteWithRequest[AddAccountRequest, StatusMessageResponse](apiV1, "addAccount", http.MethodPost, "/accounts", "Add an account", s.handleAddAccount, http.StatusOK, http.StatusCreated)
	registerAPIV1RawHumaJSONRoute[SourceStatusResponse](apiV1, "listSourceStatus", http.MethodGet, "/sources/status", "List source sync status", s.handleSourceStatus)
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
			queryIntegerParam("limit", "Maximum number of rows to return"),
			queryIntegerParam("offset", "Zero-based row offset"),
			queryStringParam("message_type", "Message type filter; repeat or comma-separate for multiple values", false),
		}, scopeParams()...)
	case "getCLIMessage", "getCLIMessageRaw":
		return []*huma.Param{queryStringParam("id", "Message numeric ID or source message ID", true)}
	case "getCLIAttachment":
		return []*huma.Param{queryStringParam("content_hash", "Attachment SHA-256 content hash", true)}
	case "getCLICollection":
		return []*huma.Param{queryStringParam("name", "Collection name", true)}
	case "buildCLICache":
		return []*huma.Param{queryBooleanParam("full_rebuild", "Rebuild all cache files from scratch")}
	case "syncCLI":
		return []*huma.Param{queryStringParam("email", "Account email or display name to sync", false)}
	case "syncFullCLI":
		return []*huma.Param{
			queryStringParam("email", "Account email or display name to sync", false),
			queryStringParam("query", "Gmail search query", false),
			queryStringParam("after", "Only messages on or after this YYYY-MM-DD date", false),
			queryStringParam("before", "Only messages before this YYYY-MM-DD date", false),
			queryIntegerParam("limit", "Maximum messages to sync"),
			queryBooleanParam("noresume", "Ignore checkpoints and start fresh"),
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
	case "listDeletions":
		return []*huma.Param{queryStringParam("status",
			"Filter manifests by status (pending, in_progress, completed, failed, cancelled)", false)}
	case "cancelDeletion":
		return []*huma.Param{pathStringParam("id", "Deletion manifest ID")}
	case "getAttachment":
		return []*huma.Param{pathIntegerParam("Attachment ID")}
	case "getAttachmentContent":
		return []*huma.Param{pathStringParam("hash", "Attachment SHA-256 content hash")}
	case "getMessageInlinePart":
		return []*huma.Param{
			pathIntegerParam("Message ID"),
			queryStringParam("cid", "Inline MIME Content-ID", true),
		}
	case "searchMessages":
		return append([]*huma.Param{
			queryStringParam("q", "Search query", true),
			queryStringParam("mode", "Search mode: fts, vector, or hybrid", false),
			queryIntegerParam("page", "One-based page number (default 1; values below 1 are clamped to 1). Non-numeric values are rejected with 400."),
			queryIntegerParam("page_size", "Page size (default 20, max 100; out-of-range values are clamped). Non-numeric values are rejected with 400."),
			queryIntegerParam("offset", "Zero-based ranking offset for vector or hybrid search (default 0)"),
			queryBooleanParam("explain", "Include score explanation when mode is vector or hybrid"),
			queryBooleanParam("include_matches", "Include scored semantic chunk excerpts for vector or hybrid results"),
			queryNumberParam("min_score", "Minimum chunk score for included excerpts; does not filter ranked messages"),
			queryStringParam("message_type", "Message type filter; repeat or comma-separate for multiple values", false),
		}, scopeParams()...)
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
	case "getGmailIDsByFilter":
		return messageFilterParams()
	case "searchMessagesByDomains":
		return []*huma.Param{
			queryStringParam("domains", "Comma-separated participant domains", true),
			queryStringParam("after", "Lower date/time bound (RFC3339 or YYYY-MM-DD)", false),
			queryStringParam("before", "Upper date/time bound (RFC3339 or YYYY-MM-DD)", false),
			queryIntegerParam("offset", "Zero-based row offset"),
			queryIntegerParam("limit", "Maximum number of rows to return"),
		}
	case "findSimilarMessages":
		return []*huma.Param{
			queryRequiredIntegerParam("message_id", "Seed message ID"),
			queryIntegerParam("limit", "Maximum number of rows to return"),
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
			queryIntegerParam("limit", "Maximum number of rows to return"),
			queryStringParam("time_granularity", "Time bucket granularity", false),
			queryIntegerParam("source_id", "Source ID"),
			queryStringParam("search_query", "Search query", false),
			queryStringParam("after", "Lower date/time bound (RFC3339 or YYYY-MM-DD)", false),
			queryStringParam("before", "Upper date/time bound (RFC3339 or YYYY-MM-DD)", false),
		}
	case "listTextConversationMessages":
		return append([]*huma.Param{pathIntegerParam("Conversation ID")}, textFilterParams()...)
	case "searchTextMessages":
		return []*huma.Param{
			queryStringParam("q", "Search query", true),
			queryIntegerParam("offset", "Zero-based row offset"),
			queryIntegerParam("limit", "Maximum number of rows to return"),
		}
	case "getTextStats":
		return []*huma.Param{
			queryIntegerParam("source_id", "Source ID"),
			queryStringParam("search_query", "Search query", false),
		}
	case "listSourceStatus":
		return []*huma.Param{queryStringParam("source_type", "Restrict to one source type", false)}
	case "triggerSync":
		return []*huma.Param{pathStringParam("account", "Account email or configured source identifier")}
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
		queryIntegerParam("limit", "Maximum number of rows to return (default 100; values below 1 fall back to the default)"),
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
		queryStringParam("recipient", "Recipient email/address filter", false),
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
		queryIntegerParam("limit", "Maximum number of rows to return (default and max 500; larger values are clamped)"),
		queryStringParam("sort", "Sort field: date, size, or subject", false),
		queryStringParam("direction", "Sort direction: asc or desc", false),
	}
}

func textFilterParams() []*huma.Param {
	return []*huma.Param{
		queryIntegerParam("source_id", "Source ID"),
		queryStringParam("contact_phone", "Sender phone/address filter", false),
		queryStringParam("contact_name", "Sender display-name filter", false),
		queryStringParam("source_type", "Source type filter", false),
		queryStringParam("label", "Label filter", false),
		queryStringParam("time_period", "Named time period", false),
		queryStringParam("time_granularity", "Time bucket granularity", false),
		queryStringParam("after", "Lower date/time bound (RFC3339 or YYYY-MM-DD)", false),
		queryStringParam("before", "Upper date/time bound (RFC3339 or YYYY-MM-DD)", false),
		queryIntegerParam("offset", "Zero-based row offset"),
		queryIntegerParam("limit", "Maximum number of rows to return"),
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
	return param(name, "path", huma.TypeString, doc, true)
}

func pathIntegerParam(doc string) *huma.Param {
	p := param("id", "path", huma.TypeInteger, doc, true)
	p.Schema.Format = "int64"
	return p
}

func queryStringParam(name, doc string, required bool) *huma.Param {
	return param(name, "query", huma.TypeString, doc, required)
}

func queryIntegerParam(name, doc string) *huma.Param {
	p := param(name, "query", huma.TypeInteger, doc, false)
	p.Schema.Format = "int64"
	return p
}

func queryIntegerArrayParam(name, doc string) *huma.Param {
	p := param(name, "query", huma.TypeArray, doc, false)
	p.Schema.Items = &huma.Schema{Type: huma.TypeInteger, Format: "int64"}
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
			"application/json": {Schema: schemaFor[T](api)},
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
				"application/json": {Schema: schemaFor[T](api)},
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
				"application/json": {Schema: &huma.Schema{OneOf: oneOf}},
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
			"application/json": {Schema: schemaFor[ErrorResponse](api)},
		},
	}
}

func schemaFor[T any](api huma.API) *huma.Schema {
	return api.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[T](), true, "")
}

func registerRawHumaRoute(api huma.API, op huma.Operation, handler http.HandlerFunc) {
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
