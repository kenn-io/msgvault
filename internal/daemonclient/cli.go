package daemonclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/msgvault/internal/accountops"
	"go.kenn.io/msgvault/internal/cacheops"
	"go.kenn.io/msgvault/internal/collectionops"
	"go.kenn.io/msgvault/internal/contentverify"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/identityops"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// CLIStats is the CLI-compatible stats response returned by /api/v1/cli/stats.
type CLIStats struct {
	Stats            *store.Stats `json:"stats"`
	ScopeLabel       string       `json:"scope_label,omitempty"`
	ScopeSourceCount int          `json:"scope_source_count,omitempty"`
}

type CLICacheStats = cacheops.CacheStats

type CLISyncRequest struct {
	Full     bool
	Email    string
	Query    string
	NoResume bool
	Before   string
	After    string
	Limit    int
}

type CLIVerifyRequest struct {
	Email       string
	SampleSize  int
	SkipDBCheck bool
	JSON        bool
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

type CLIAddCalendarPlan struct {
	NeedsScopeEscalation bool     `json:"needs_scope_escalation"`
	Headline             string   `json:"headline,omitempty"`
	BodyLines            []string `json:"body_lines,omitempty"`
	CancelHint           string   `json:"cancel_hint,omitempty"`
	OAuthApp             string   `json:"oauth_app,omitempty"`
	OAuthAppResolved     bool     `json:"oauth_app_resolved,omitempty"`
	NeedsClientCheck     bool     `json:"needs_client_check,omitempty"`
}

type CLIEmbeddingsPlanRequest struct {
	Operation    string `json:"operation"`
	GenerationID int64  `json:"generation_id"`
	Force        bool   `json:"force,omitempty"`
}

type CLIEmbeddingsPlan struct {
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

type CLIDeleteStagedPlan struct {
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
	ScopeEscalationAccount    string   `json:"scope_escalation_account,omitempty"`
	ScopeEscalationOAuthApp   string   `json:"scope_escalation_oauth_app,omitempty"`
	BlockedError              string   `json:"blocked_error,omitempty"`
	RemoteDeleteEnvVar        string   `json:"remote_delete_env_var,omitempty"`
}

type CLIDeletionManifestResult struct {
	ID           string
	MessageCount int
}

type CLIDeduplicatePlanRequest struct {
	Account                    string
	Collection                 string
	Prefer                     string
	ContentHash                bool
	DeleteDupsFromSourceServer bool
}

type CLIDeduplicatePlan struct {
	PrefixStdout string
	Items        []CLIDeduplicatePlanItem
	FooterStdout string
}

type CLIDeduplicatePlanItem struct {
	SourceID          int64
	ScopeLabel        string
	ScopeIsCollection bool
	Stdout            string
	DuplicateMessages int
	BackfilledCount   int64
	PlanFingerprint   string
	NeedsConfirmation bool
}

type CLIInitDB struct {
	Stats  *store.Stats `json:"stats"`
	Notice string       `json:"notice,omitempty"`
}

type CLISearchRequest struct {
	Query        string
	Account      string
	Collection   string
	MessageTypes []string
	Limit        int
	Offset       int
}

type CLISearch struct {
	Results          []query.MessageSummary `json:"results"`
	ScopeLabel       string                 `json:"scope_label,omitempty"`
	ScopeSourceCount int                    `json:"scope_source_count,omitempty"`
	// IndexBuilt/IndexedMessages come from pre-0.18 daemons that built the
	// FTS index synchronously inside the search request. Current daemons
	// build in the background and report IndexState ("checking" or
	// "building") instead.
	IndexBuilt      bool   `json:"index_built,omitempty"`
	IndexedMessages int64  `json:"indexed_messages,omitempty"`
	IndexState      string `json:"index_state,omitempty"`
}

type CLIHybridSearchRequest struct {
	Query          string
	Account        string
	Collection     string
	MessageTypes   []string
	Mode           string
	Limit          int
	Offset         int
	IncludeMatches bool
	MinScore       float64
}

type CLIHybridSearch struct {
	Results          []CLIHybridSearchResult
	Generation       CLIHybridGeneration
	PoolSaturated    bool
	ReturnedCount    int
	ScopeLabel       string
	ScopeSourceCount int
	HasMore          bool
}

type CLIHybridGeneration struct {
	ID          int64
	Model       string
	Dimension   int
	Fingerprint string
	State       string
}

type CLIHybridSearchResult struct {
	ID               int64
	Subject          string
	FromEmail        string
	SentAt           time.Time
	RRFScore         *float64
	BM25Score        *float64
	VectorScore      *float64
	SubjectBoosted   bool
	Matches          []CLIHybridSearchMatch
	MatchesTruncated bool
}

type CLIHybridSearchMatch struct {
	CharOffset *int
	Snippet    string
	Line       *int
	Score      float64
}

type SimilarSearchRequest struct {
	MessageID     int64
	Limit         int
	Account       string
	MessageType   string
	After         *time.Time
	Before        *time.Time
	HasAttachment *bool
}

type SimilarSearch struct {
	SeedMessageID int64
	Returned      int
	Generation    CLIHybridGeneration
	Messages      []query.MessageSummary
}

type CLIAccount struct {
	ID                 int64      `json:"id"`
	Email              string     `json:"email"`
	Type               string     `json:"type"`
	DisplayName        string     `json:"display_name"`
	OAuthApp           string     `json:"oauth_app,omitempty"`
	MessageCount       int64      `json:"message_count"`
	SourceDeletedCount int64      `json:"source_deleted_count"`
	LastSync           *time.Time `json:"last_sync"`
}

type CLIAccountUpdateRequest = accountops.UpdateRequest
type CLIAccountUpdateResult = accountops.UpdateResult

type CLICollection struct {
	ID                 int64                 `json:"id"`
	Name               string                `json:"name"`
	Description        string                `json:"description,omitempty"`
	CreatedAt          time.Time             `json:"created_at"`
	SourceIDs          []int64               `json:"source_ids"`
	MessageCount       int64                 `json:"message_count"`
	SourceDeletedCount int64                 `json:"source_deleted_count"`
	Sources            []CLICollectionSource `json:"sources,omitempty"`
}

type CLICollectionSource struct {
	ID          int64  `json:"id"`
	Identifier  string `json:"identifier"`
	DisplayName string `json:"display_name,omitempty"`
}

// CLICollectionCreateRequest creates a collection through the CLI API.
type CLICollectionCreateRequest = collectionops.CreateRequest

// CLICollectionSourcesRequest adds or removes accounts from a CLI collection.
type CLICollectionSourcesRequest = collectionops.SourcesRequest

// CLICollectionMutationResult is returned by CLI collection mutations.
type CLICollectionMutationResult = collectionops.MutationResult

type CLIIdentityRow struct {
	Account     string     `json:"account"`
	SourceID    int64      `json:"source_id"`
	SourceType  string     `json:"source_type"`
	Identifier  string     `json:"identifier,omitempty"`
	Signals     []string   `json:"signals"`
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
	None        bool       `json:"none,omitempty"`
}

// CLIIdentitiesRequest describes the CLI identity rows to fetch.
type CLIIdentitiesRequest struct {
	Account     string
	Collection  string
	PrimaryOnly bool
}

type CLIIdentityAddRequest = identityops.AddRequest
type CLIIdentityAddResult = identityops.AddResult
type CLIIdentityRemoveRequest = identityops.RemoveRequest
type CLIIdentityRemoveResult = identityops.RemoveResult

type CLIDeleteDedupedRequest struct {
	BatchIDs           []string
	AllHidden          bool
	NoBackup           bool
	ExpectedTotal      *int64
	ExpectedBatchCount *int64
	ExpectedBatches    []CLIDeleteDedupedBatch
}

type CLIDeleteDedupedBatch struct {
	ID    string
	Count int64
}

type CLIDeleteDedupedPlan struct {
	Total      int64
	BatchCount int64
	Batches    []CLIDeleteDedupedBatch
}

type CLIDeleteDedupedExecute struct {
	Deleted    int64
	BatchCount int64
	BackupPath string
}

type cliRebuildFTSEvent struct {
	Type    string `json:"type"`
	Done    int64  `json:"done,omitempty"`
	Total   int64  `json:"total,omitempty"`
	Indexed int64  `json:"indexed,omitempty"`
	Error   string `json:"error,omitempty"`
}

type cliStreamEvent struct {
	Type  string `json:"type"`
	Data  string `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

const (
	apiErrorCodeMessageNotFound = "message_not_found"
	apiErrorCodeLegacyNotFound  = "not_found"
)

// InitCLIArchive runs setup-style startup work through the CLI-compatible API.
func (c *Client) InitCLIArchive(ctx context.Context) (*CLIInitDB, error) {
	resp, err := CLIResponse(c, func(client *apiclient.Client) (*generated.InitCLIArchiveResp, error) {
		return client.InitCLIArchiveWithResponse(ctx)
	})
	if err != nil {
		return nil, err
	}
	return cliInitDBFromGenerated(resp.JSON200), nil
}

// GetCLICacheStats fetches analytics cache statistics through the daemon.
func (c *Client) GetCLICacheStats(ctx context.Context) (*CLICacheStats, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.GetCLICacheStatsResp, error) {
		return client.GetCLICacheStatsWithResponse(ctx)
	})
	if err != nil {
		return nil, err
	}
	return cliCacheStatsFromGenerated(resp.JSON200), nil
}

func (c *Client) BuildCLICache(
	ctx context.Context,
	fullRebuild bool,
	output func(stream, data string) error,
) error {
	options := &generated.BuildCLICacheRequestOptions{}
	if fullRebuild {
		options.Query = &generated.BuildCLICacheQuery{FullRebuild: optionalBool(true)}
	}
	return c.runCLIStream(ctx, "/api/v1/cli/build-cache", "build-cache", options, output)
}

func (c *Client) RunCLISync(
	ctx context.Context,
	req CLISyncRequest,
	output func(stream, data string) error,
) error {
	path := "/api/v1/cli/sync"
	if req.Full {
		path = "/api/v1/cli/sync-full"
		return c.runCLIStream(ctx, path, "sync", &generated.SyncFullCLIRequestOptions{
			Query: &generated.SyncFullCLIQuery{
				Email:    optionalString(req.Email),
				Query:    optionalString(req.Query),
				Noresume: optionalBool(req.NoResume),
				Before:   optionalString(req.Before),
				After:    optionalString(req.After),
				Limit:    optionalPositiveInt64(req.Limit),
			},
		}, output)
	}
	return c.runCLIStream(ctx, path, "sync", &generated.SyncCLIRequestOptions{
		Query: &generated.SyncCLIQuery{
			Email: optionalString(req.Email),
		},
	}, output)
}

func (c *Client) RunCLIVerify(
	ctx context.Context,
	req CLIVerifyRequest,
	output func(stream, data string) error,
) error {
	sampleSize := int64(req.SampleSize)
	return c.runCLIStream(ctx, "/api/v1/cli/verify", "verify", &generated.VerifyCLIRequestOptions{
		Query: &generated.VerifyCLIQuery{
			Email:       req.Email,
			Sample:      &sampleSize,
			SkipDBCheck: optionalBool(req.SkipDBCheck),
			JSON:        optionalBool(req.JSON),
		},
	}, output)
}

func (c *Client) RunCLIRepairEncoding(
	ctx context.Context,
	output func(stream, data string) error,
) error {
	return c.runCLIStream(ctx, "/api/v1/cli/repair-encoding", "repair-encoding", nil, output)
}

func (c *Client) RunCLICommand(
	ctx context.Context,
	req CLIRunRequest,
	output func(stream, data string) error,
) error {
	body := generated.RunCLIBody{Args: req.Args, Env: req.Env, Cwd: optionalString(req.Cwd)}
	return c.runCLIStream(ctx, "/api/v1/cli/run", "run", &generated.RunCLIRequestOptions{Body: &body}, output)
}

func (c *Client) PlanCLIAddCalendar(
	ctx context.Context,
	req CLIAddCalendarPlanRequest,
) (*CLIAddCalendarPlan, error) {
	body := generated.PlanCLIAddCalendarBody{
		Email:            req.Email,
		OauthApp:         optionalString(req.OAuthApp),
		OauthAppExplicit: optionalBool(req.OAuthAppExplicit),
		Headless:         optionalBool(req.Headless),
	}
	resp, err := CLIResponse(c, func(client *apiclient.Client) (*generated.PlanCLIAddCalendarResp, error) {
		return client.PlanCLIAddCalendarWithResponse(ctx, &generated.PlanCLIAddCalendarRequestOptions{Body: &body})
	})
	if err != nil {
		return nil, err
	}
	return cliAddCalendarPlanFromGenerated(resp.JSON200), nil
}

func (c *Client) PlanCLIEmbeddings(
	ctx context.Context,
	req CLIEmbeddingsPlanRequest,
) (*CLIEmbeddingsPlan, error) {
	body := generated.PlanCLIEmbeddingsBody{
		Operation:    req.Operation,
		GenerationID: req.GenerationID,
		Force:        optionalBool(req.Force),
	}
	resp, err := CLIResponse(c, func(client *apiclient.Client) (*generated.PlanCLIEmbeddingsResp, error) {
		return client.PlanCLIEmbeddingsWithResponse(ctx, &generated.PlanCLIEmbeddingsRequestOptions{Body: &body})
	})
	if err != nil {
		return nil, err
	}
	return cliEmbeddingsPlanFromGenerated(resp.JSON200), nil
}

func (c *Client) PlanCLIDeleteStaged(
	ctx context.Context,
	req CLIDeleteStagedPlanRequest,
) (*CLIDeleteStagedPlan, error) {
	body := generated.PlanCLIDeleteStagedBody{
		Account:             optionalString(req.Account),
		BatchID:             optionalString(req.BatchID),
		DryRun:              optionalBool(req.DryRun),
		List:                optionalBool(req.List),
		Permanent:           optionalBool(req.Permanent),
		RemoteDeleteEnabled: optionalBool(req.RemoteDeleteEnabled),
		Yes:                 optionalBool(req.Yes),
	}
	resp, err := CLIResponse(c, func(client *apiclient.Client) (*generated.PlanCLIDeleteStagedResp, error) {
		return client.PlanCLIDeleteStagedWithResponse(ctx, &generated.PlanCLIDeleteStagedRequestOptions{Body: &body})
	})
	if err != nil {
		return nil, err
	}
	return cliDeleteStagedPlanFromGenerated(resp.JSON200), nil
}

func (c *Client) CreateCLIDeletionManifest(
	ctx context.Context,
	manifest *deletion.Manifest,
) (*CLIDeletionManifestResult, error) {
	if manifest == nil {
		return nil, errors.New("missing deletion manifest")
	}
	body := cliDeletionManifestToGenerated(manifest)
	resp, err := CLIResponse(c, func(client *apiclient.Client) (*generated.CreateCLIDeletionManifestResp, error) {
		return client.CreateCLIDeletionManifestWithResponse(ctx, &generated.CreateCLIDeletionManifestRequestOptions{
			Body: &body,
		})
	})
	if err != nil {
		return nil, err
	}
	return cliDeletionManifestResultFromGenerated(resp.JSON200), nil
}

// SaveManifest adapts Client to deletion manifest saver interfaces.
func (c *Client) SaveManifest(manifest *deletion.Manifest) error {
	_, err := c.CreateCLIDeletionManifest(context.Background(), manifest)
	return err
}

func (c *Client) PlanCLIDeduplicate(
	ctx context.Context,
	req CLIDeduplicatePlanRequest,
) (*CLIDeduplicatePlan, error) {
	body := generated.PlanCLIDeduplicateBody{
		Account:                    optionalString(req.Account),
		Collection:                 optionalString(req.Collection),
		Prefer:                     optionalString(req.Prefer),
		ContentHash:                optionalBool(req.ContentHash),
		DeleteDupsFromSourceServer: optionalBool(req.DeleteDupsFromSourceServer),
	}
	resp, err := CLIResponse(c, func(client *apiclient.Client) (*generated.PlanCLIDeduplicateResp, error) {
		return client.PlanCLIDeduplicateWithResponse(ctx, &generated.PlanCLIDeduplicateRequestOptions{Body: &body})
	})
	if err != nil {
		return nil, err
	}
	return cliDeduplicatePlanFromGenerated(resp.JSON200), nil
}

// openCLIStream POSTs to a streaming CLI endpoint, retrying while the daemon
// reports another operation in progress, and returns the open response body.
func (c *Client) openCLIStream(
	ctx context.Context,
	path string,
	options runtime.RequestOptions,
) (*http.Response, error) {
	waiter := &operationBusyWaiter{c: c}
	for {
		resp, err := c.DoGeneratedStreamingRequestWithContext(ctx, http.MethodPost, path, options)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		err = HandleCLIErrorResponse(resp)
		_ = resp.Body.Close()
		if waiter.wait(ctx, err) {
			continue
		}
		return nil, err
	}
}

func (c *Client) runCLIStream(
	ctx context.Context,
	path string,
	operation string,
	options runtime.RequestOptions,
	output func(stream, data string) error,
) error {
	resp, err := c.openCLIStream(ctx, path, options)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	return decodeCLIStream(resp.Body, operation, func(event cliStreamEvent) (bool, error) {
		switch event.Type {
		case "stdout", "stderr":
			if output != nil && event.Data != "" {
				if err := output(event.Type, event.Data); err != nil {
					return false, err
				}
			}
		case "complete":
			return true, nil
		case "error":
			if event.Error != "" {
				return false, errors.New(event.Error)
			}
			return false, fmt.Errorf("%s failed", operation)
		default:
			return false, nil
		}
		return false, nil
	})
}

// GetCLIStats fetches archive statistics using the CLI-compatible API.
// account and collection are mutually exclusive; leave both empty for global
// archive stats.
func (c *Client) GetCLIStats(
	ctx context.Context,
	account string,
	collection string,
) (*CLIStats, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.GetCLIStatsResp, error) {
		return client.GetCLIStatsWithResponse(ctx, &generated.GetCLIStatsRequestOptions{
			Query: &generated.GetCLIStatsQuery{
				Account:    optionalString(account),
				Collection: optionalString(collection),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return cliStatsFromGenerated(resp.JSON200), nil
}

func (c *Client) GetCLISearch(ctx context.Context, req CLISearchRequest) (*CLISearch, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.SearchCLIResp, error) {
		return client.SearchCLIWithResponse(ctx, &generated.SearchCLIRequestOptions{
			Query: &generated.SearchCLIQuery{
				Q:           req.Query,
				Account:     optionalString(req.Account),
				Collection:  optionalString(req.Collection),
				MessageType: optionalMessageTypes(req.MessageTypes),
				Limit:       optionalPositiveInt64(req.Limit),
				Offset:      optionalPositiveInt64(req.Offset),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return cliSearchFromGenerated(resp.JSON200), nil
}

func (c *Client) GetCLIHybridSearch(
	ctx context.Context,
	req CLIHybridSearchRequest,
) (*CLIHybridSearch, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.SearchMessagesResp, error) {
		return client.SearchMessagesWithResponse(ctx, &generated.SearchMessagesRequestOptions{
			Query: &generated.SearchMessagesQuery{
				Q:              req.Query,
				Mode:           optionalString(req.Mode),
				Explain:        optionalBool(true),
				Account:        optionalString(req.Account),
				Collection:     optionalString(req.Collection),
				MessageType:    optionalMessageTypes(req.MessageTypes),
				PageSize:       optionalPositiveInt64(req.Limit),
				Offset:         optionalPositiveInt64(req.Offset),
				IncludeMatches: optionalBool(req.IncludeMatches),
				MinScore:       optionalFloat32(req.MinScore),
			},
		})
	})
	if err != nil {
		return nil, err
	}

	// The generated oneOf wrapper can decode this payload as the FTS arm
	// because both schemas are object-shaped. This adapter only serves
	// vector/hybrid CLI calls, so decode the concrete generated type.
	hybridResp, err := DecodeGeneratedSearchBody[generated.HybridSearchResponse]("CLI hybrid search", resp.Body)
	if err != nil {
		return nil, err
	}
	return cliHybridSearchFromGenerated(hybridResp)
}

func (c *Client) FindSimilarMessages(
	ctx context.Context,
	req SimilarSearchRequest,
) (*SimilarSearch, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.FindSimilarMessagesResp, error) {
		return client.FindSimilarMessagesWithResponse(ctx, &generated.FindSimilarMessagesRequestOptions{
			Query: &generated.FindSimilarMessagesQuery{
				MessageID:     req.MessageID,
				Limit:         optionalPositiveInt64(req.Limit),
				Account:       optionalString(req.Account),
				MessageType:   optionalString(req.MessageType),
				After:         optionalTimeRFC3339(req.After),
				Before:        optionalTimeRFC3339(req.Before),
				HasAttachment: req.HasAttachment,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	if resp.JSON200 == nil {
		return &SimilarSearch{}, nil
	}
	return &SimilarSearch{
		SeedMessageID: resp.JSON200.SeedMessageID,
		Returned:      int(resp.JSON200.Returned),
		Generation: CLIHybridGeneration{
			ID:          resp.JSON200.Generation.ID,
			Model:       resp.JSON200.Generation.Model,
			Dimension:   int(resp.JSON200.Generation.Dimension),
			Fingerprint: resp.JSON200.Generation.Fingerprint,
			State:       resp.JSON200.Generation.State,
		},
		Messages: messageSummariesFromGenerated(resp.JSON200.Messages),
	}, nil
}

func (c *Client) GetCLIAccounts(ctx context.Context) ([]CLIAccount, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.ListCLIAccountsResp, error) {
		return client.ListCLIAccountsWithResponse(ctx)
	})
	if err != nil {
		return nil, err
	}
	return cliAccountsFromGenerated(resp.JSON200), nil
}

func (c *Client) UpdateCLIAccount(
	ctx context.Context,
	req CLIAccountUpdateRequest,
) (*CLIAccountUpdateResult, error) {
	resp, err := CLIResponse(c, func(client *apiclient.Client) (*generated.UpdateCLIAccountResp, error) {
		return client.UpdateCLIAccountWithResponse(ctx, &generated.UpdateCLIAccountRequestOptions{
			Body: &generated.UpdateCLIAccountBody{
				Email:       req.Email,
				DisplayName: req.DisplayName,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return cliAccountUpdateResultFromGenerated(resp.JSON200), nil
}

func (c *Client) GetCLICollections(ctx context.Context) ([]CLICollection, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.ListCLICollectionsResp, error) {
		return client.ListCLICollectionsWithResponse(ctx)
	})
	if err != nil {
		return nil, err
	}
	return cliCollectionsFromGenerated(resp.JSON200), nil
}

func (c *Client) GetCLICollection(ctx context.Context, name string) (*CLICollection, error) {
	resp, err := APIResponseWithNotFound(
		c,
		func(client *apiclient.Client) (*generated.GetCLICollectionResp, error) {
			return client.GetCLICollectionWithResponse(ctx, &generated.GetCLICollectionRequestOptions{
				Query: &generated.GetCLICollectionQuery{Name: name},
			})
		},
		func(*generated.GetCLICollectionResp) error {
			return store.ErrCollectionNotFound
		},
	)
	if err != nil {
		return nil, err
	}
	collection := cliCollectionFromGenerated(resp.JSON200.Collection)
	return &collection, nil
}

func (c *Client) CreateCLICollection(
	ctx context.Context,
	req CLICollectionCreateRequest,
) (*CLICollectionMutationResult, error) {
	resp, err := CLIResponse(c, func(client *apiclient.Client) (*generated.CreateCLICollectionResp, error) {
		return client.CreateCLICollectionWithResponse(ctx, &generated.CreateCLICollectionRequestOptions{
			Body: &generated.CreateCLICollectionBody{
				Name:     req.Name,
				Accounts: req.Accounts,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return cliCollectionMutationResultFromGenerated(resp.JSON200), nil
}

func (c *Client) AddCLICollectionSources(
	ctx context.Context,
	name string,
	req CLICollectionSourcesRequest,
) (*CLICollectionMutationResult, error) {
	resp, err := CLIResponse(c, func(client *apiclient.Client) (*generated.AddCLICollectionSourcesResp, error) {
		return client.AddCLICollectionSourcesWithResponse(ctx, &generated.AddCLICollectionSourcesRequestOptions{
			PathParams: &generated.AddCLICollectionSourcesPath{Name: url.PathEscape(name)},
			Body:       &generated.AddCLICollectionSourcesBody{Accounts: req.Accounts},
		})
	})
	if err != nil {
		return nil, err
	}
	return cliCollectionMutationResultFromGenerated(resp.JSON200), nil
}

func (c *Client) RemoveCLICollectionSources(
	ctx context.Context,
	name string,
	req CLICollectionSourcesRequest,
) (*CLICollectionMutationResult, error) {
	resp, err := CLIResponse(c, func(client *apiclient.Client) (*generated.RemoveCLICollectionSourcesResp, error) {
		return client.RemoveCLICollectionSourcesWithResponse(ctx, &generated.RemoveCLICollectionSourcesRequestOptions{
			PathParams: &generated.RemoveCLICollectionSourcesPath{Name: url.PathEscape(name)},
			Body:       &generated.RemoveCLICollectionSourcesBody{Accounts: req.Accounts},
		})
	})
	if err != nil {
		return nil, err
	}
	return cliCollectionMutationResultFromGenerated(resp.JSON200), nil
}

func (c *Client) DeleteCLICollection(
	ctx context.Context,
	name string,
) (*CLICollectionMutationResult, error) {
	resp, err := CLIResponse(c, func(client *apiclient.Client) (*generated.DeleteCLICollectionResp, error) {
		return client.DeleteCLICollectionWithResponse(ctx, &generated.DeleteCLICollectionRequestOptions{
			PathParams: &generated.DeleteCLICollectionPath{Name: url.PathEscape(name)},
		})
	})
	if err != nil {
		return nil, err
	}
	return cliCollectionMutationResultFromGenerated(resp.JSON200), nil
}

func (c *Client) GetCLIIdentities(
	ctx context.Context,
	req CLIIdentitiesRequest,
) ([]CLIIdentityRow, error) {
	resp, err := CLIResponse(c, func(client *apiclient.Client) (*generated.ListCLIIdentitiesResp, error) {
		return client.ListCLIIdentitiesWithResponse(ctx, &generated.ListCLIIdentitiesRequestOptions{
			Query: &generated.ListCLIIdentitiesQuery{
				Account:     optionalString(req.Account),
				Collection:  optionalString(req.Collection),
				PrimaryOnly: optionalBool(req.PrimaryOnly),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return cliIdentitiesFromGenerated(resp.JSON200), nil
}

func (c *Client) AddCLIIdentity(
	ctx context.Context,
	req CLIIdentityAddRequest,
) (*CLIIdentityAddResult, error) {
	resp, err := CLIResponse(c, func(client *apiclient.Client) (*generated.AddCLIIdentityResp, error) {
		return client.AddCLIIdentityWithResponse(ctx, &generated.AddCLIIdentityRequestOptions{
			Body: &generated.AddCLIIdentityBody{
				Account:    req.Account,
				Identifier: req.Identifier,
				Signal:     req.Signal,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return cliIdentityAddResultFromGenerated(resp.JSON200), nil
}

func (c *Client) RemoveCLIIdentity(
	ctx context.Context,
	req CLIIdentityRemoveRequest,
) (*CLIIdentityRemoveResult, error) {
	resp, err := CLIResponse(c, func(client *apiclient.Client) (*generated.RemoveCLIIdentityResp, error) {
		return client.RemoveCLIIdentityWithResponse(ctx, &generated.RemoveCLIIdentityRequestOptions{
			Body: &generated.RemoveCLIIdentityBody{
				Account:    req.Account,
				Identifier: req.Identifier,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return cliIdentityRemoveResultFromGenerated(resp.JSON200), nil
}

func (c *Client) PlanCLIDeleteDeduped(
	ctx context.Context,
	req CLIDeleteDedupedRequest,
) (*CLIDeleteDedupedPlan, error) {
	resp, err := CLIResponse(c, func(client *apiclient.Client) (*generated.PlanCLIDeleteDedupedResp, error) {
		return client.PlanCLIDeleteDedupedWithResponse(ctx, &generated.PlanCLIDeleteDedupedRequestOptions{
			Body: cliDeleteDedupedPlanBodyFromRequest(req),
		})
	})
	if err != nil {
		return nil, err
	}
	return cliDeleteDedupedPlanFromGenerated(resp.JSON200), nil
}

func (c *Client) ExecuteCLIDeleteDeduped(
	ctx context.Context,
	req CLIDeleteDedupedRequest,
) (*CLIDeleteDedupedExecute, error) {
	resp, err := CLIResponse(c, func(client *apiclient.Client) (*generated.ExecuteCLIDeleteDedupedResp, error) {
		return client.ExecuteCLIDeleteDedupedWithResponse(ctx, &generated.ExecuteCLIDeleteDedupedRequestOptions{
			Body: cliDeleteDedupedExecuteBodyFromRequest(req),
		})
	})
	if err != nil {
		return nil, err
	}
	return cliDeleteDedupedExecuteFromGenerated(resp.JSON200), nil
}

func (c *Client) RebuildCLIFTS(
	ctx context.Context,
	progress func(done, total int64),
) (int64, error) {
	resp, err := c.openCLIStream(ctx, "/api/v1/cli/rebuild-fts", nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	var indexed int64
	err = decodeCLIStream(resp.Body, "rebuild FTS", func(event cliRebuildFTSEvent) (bool, error) {
		switch event.Type {
		case "progress":
			if progress != nil {
				progress(event.Done, event.Total)
			}
		case "complete":
			indexed = event.Indexed
			return true, nil
		case "error":
			if event.Error != "" {
				return false, errors.New(event.Error)
			}
			return false, errors.New("rebuild FTS failed")
		default:
			return false, nil
		}
		return false, nil
	})
	if err != nil {
		return 0, err
	}
	return indexed, nil
}

func (c *Client) GetCLIMessage(ctx context.Context, id string) (*query.MessageDetail, error) {
	resp, err := APIResponseWithNotFound(
		c,
		func(client *apiclient.Client) (*generated.GetCLIMessageResp, error) {
			return client.GetCLIMessageWithResponse(ctx, &generated.GetCLIMessageRequestOptions{
				Query: &generated.GetCLIMessageQuery{ID: id},
			})
		},
		func(*generated.GetCLIMessageResp) error {
			return fmt.Errorf("message %s: %w", id, store.ErrMessageNotFound)
		},
	)
	if err != nil {
		return nil, err
	}
	return cliMessageDetailFromGenerated(resp.JSON200), nil
}

func (c *Client) GetCLIMessageRaw(ctx context.Context, id string) ([]byte, string, error) {
	resp, err := APIResponseWithNotFound(
		c,
		func(client *apiclient.Client) (*generated.GetCLIMessageRawResp, error) {
			return client.GetCLIMessageRawWithResponse(ctx, &generated.GetCLIMessageRawRequestOptions{
				Query: &generated.GetCLIMessageRawQuery{ID: id},
			})
		},
		func(resp *generated.GetCLIMessageRawResp) error {
			return handleCLIMessageRawNotFound(resp, id)
		},
	)
	if err != nil {
		return nil, "", err
	}

	sourceMessageID := ""
	if resp.HTTPResponse != nil {
		sourceMessageID = resp.HTTPResponse.Header.Get("X-Msgvault-Source-Message-Id")
	}
	return resp.Body, sourceMessageID, nil
}

func handleCLIMessageRawNotFound(resp *generated.GetCLIMessageRawResp, id string) error {
	if resp == nil {
		return fmt.Errorf("API error (%d)", http.StatusNotFound)
	}
	if resp.JSON404 != nil {
		message := ""
		if resp.JSON404.Message != nil {
			message = *resp.JSON404.Message
		}
		if resp.JSON404.ErrorData == apiErrorCodeMessageNotFound ||
			(resp.JSON404.ErrorData == apiErrorCodeLegacyNotFound && message == "Message not found") {
			return fmt.Errorf("message %s: %w", id, store.ErrMessageNotFound)
		}
		if message != "" {
			return fmt.Errorf("API error (%d): %s", http.StatusNotFound, message)
		}
	}
	if len(resp.Body) == 0 {
		return fmt.Errorf("API error (%d): ", http.StatusNotFound)
	}
	return fmt.Errorf("API error (%d): %s", http.StatusNotFound, string(resp.Body))
}

func (c *Client) GetCLIAttachment(ctx context.Context, contentHash string) ([]byte, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.GetCLIAttachmentResp, error) {
		return client.GetCLIAttachmentWithResponse(ctx, &generated.GetCLIAttachmentRequestOptions{
			Query: &generated.GetCLIAttachmentQuery{ContentHash: contentHash},
		})
	})
	if err != nil {
		return nil, err
	}
	if err := contentverify.VerifyBytes(resp.Body, contentHash); err != nil {
		return nil, fmt.Errorf("verify downloaded attachment %s: %w", contentHash, err)
	}
	return resp.Body, nil
}

func (c *Client) ReadAttachment(ctx context.Context, contentHash string) ([]byte, error) {
	return c.GetCLIAttachment(ctx, contentHash)
}

func (c *Client) OpenCLIAttachment(ctx context.Context, contentHash string) (io.ReadCloser, error) {
	resp, err := c.DoGeneratedRequestWithContext(
		ctx,
		http.MethodGet,
		"/api/v1/cli/attachment",
		&generated.GetCLIAttachmentRequestOptions{
			Query: &generated.GetCLIAttachmentQuery{ContentHash: contentHash},
		},
	)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("attachment not found: %s", contentHash)
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, HandleErrorResponse(resp)
	}
	verified, err := contentverify.NewReadCloser(resp.Body, contentHash)
	if err != nil {
		return nil, errors.Join(err, resp.Body.Close())
	}
	return verified, nil
}

func (c *Client) RunSQLQuery(ctx context.Context, sql string) (*query.QueryResult, error) {
	// CLIResponse surfaces the daemon's user-facing message (e.g. the read-only
	// guard rejection) directly, without the "API error (400)" wrapper.
	resp, err := CLIResponse(c, func(client *apiclient.Client) (*generated.RunQueryResp, error) {
		return client.RunQueryWithResponse(ctx, &generated.RunQueryRequestOptions{
			Body: &generated.RunQueryBody{SQL: sql},
		})
	})
	if err != nil {
		return nil, err
	}
	return queryResultFromBody(resp.Body)
}

// queryResultFromBody re-decodes the raw response body with UseNumber so
// numeric cells stay json.Number. The generated client's plain
// json.Unmarshal turns every number into float64, which loses precision
// above 2^53 and renders in scientific notation.
func queryResultFromBody(body []byte) (*query.QueryResult, error) {
	if len(body) == 0 {
		return &query.QueryResult{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var result query.QueryResult
	if err := dec.Decode(&result); err != nil {
		return nil, fmt.Errorf("decode query result: %w", err)
	}
	return &result, nil
}

func cliAccountUpdateResultFromGenerated(result *generated.UpdateResult) *CLIAccountUpdateResult {
	if result == nil {
		return &CLIAccountUpdateResult{}
	}
	return &CLIAccountUpdateResult{
		Email:       result.Email,
		DisplayName: result.DisplayName,
	}
}
