package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/deletion"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/vector/visual"
	"go.kenn.io/msgvault/pkg/client/generated"
)

var mcpForceSQL bool
var mcpNoSQLiteScanner bool
var mcpHTTPAddr string
var mcpHTTPAllowInsecure bool
var mcpHTTPAllowWrites bool
var mcpAllowProfileWrites bool
var mcpAllowIdentityDecisions bool
var mcpAllowIdentityScoring bool
var mcpAllowPersonMerges bool
var mcpAllowCardDAVWrites bool
var mcpAllowSourceWrites bool
var mcpAllowDraftWrites bool
var mcpAllowProviderWrites bool
var mcpAllowDocumentWrites bool
var mcpAllowCardDAVOperationWrites bool
var mcpAllowVisualWrites bool
var mcpAllowSettingsWrites bool
var mcpAllowEnrichmentWrites bool
var mcpAllowInferenceRuns bool
var mcpDocumentCapabilities string
var serveMCPHTTPWithOptions = mcpserver.ServeHTTPWithOptions

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Run MCP server for Claude Desktop integration",
	Long: `Start an MCP (Model Context Protocol) server over stdio.

This allows Claude Desktop (or any MCP client) to query your archive
using tools like search_metadata, search_message_bodies, search_document_attachments, semantic_search_messages, get_message, list_messages, list_thread, export_eml, get_stats,
aggregate, get_person_agenda, list_saved_views, run_saved_view, and stage_deletion.

Add to Claude Desktop config:
  {
    "mcpServers": {
      "msgvault": {
        "command": "msgvault",
        "args": ["mcp"]
      }
	    }
	  }`,
	RunE: func(cmd *cobra.Command, args []string) error {
		state := invocationFromCommand(cmd)
		if state == nil {
			return errors.New("invocation state is unavailable")
		}
		delegated := isAgentMode(state)
		if delegated && mcpHTTPAddr != "" {
			return errors.New("agent-delegated MCP supports stdio only")
		}
		if !delegated && state.cfg == nil {
			return errors.New("configuration is unavailable")
		}
		cfg := state.cfg
		var st *daemonclient.Client
		var info HTTPStoreInfo
		var err error
		if delegated {
			st, err = openMCPAgentDelegatedStore(cmd.Context(), state)
		} else {
			st, info, err = OpenHTTPStore(cmd.Context())
		}
		if err != nil {
			return fmt.Errorf("open daemon: %w", err)
		}
		defer func() { _ = st.Close() }()
		manifest, err := resolveMCPDocumentManifest(mcpDocumentCapabilities, info, delegated)
		if err != nil {
			return usageErr(cmd, err)
		}

		// Derive from cmd.Context() so signal handling installed by
		// the cobra root command (SIGINT/SIGTERM → ctx.Done()) reaches
		// the MCP transport and can trigger ServeHTTPWithOptions's
		// graceful shutdown.
		ctx, cancel := context.WithCancel(cmd.Context())
		defer cancel()

		opts := daemonMCPServeOptions(ctx, st, state)
		if backend, ok := opts.Operations.(*daemonMCPOperations); ok && manifest != "" {
			backend.useDocumentManifest(manifest)
			opts.OperationCapabilities = backend.capabilities()
		}
		opts.AllowProfileWrites = mcpAllowProfileWrites
		opts.AllowIdentityDecisions = mcpAllowIdentityDecisions
		opts.AllowIdentityScoring = mcpAllowIdentityScoring
		opts.AllowPersonMerges = mcpAllowPersonMerges
		opts.AllowCardDAVWrites = mcpAllowCardDAVWrites
		if mcpAllowProfileWrites {
			opts.OperationWriteFamilies = append(opts.OperationWriteFamilies, mcpserver.OperationFamilyRecords)
		}
		if mcpAllowInferenceRuns {
			opts.OperationWriteFamilies = append(opts.OperationWriteFamilies, mcpserver.OperationFamilyInference)
		}
		if mcpAllowSourceWrites {
			opts.OperationWriteFamilies = append(opts.OperationWriteFamilies, mcpserver.OperationFamilySources)
		}

		if mcpAllowDraftWrites {
			opts.OperationWriteFamilies = append(opts.OperationWriteFamilies, mcpserver.OperationFamilyDrafts)
		}

		if mcpAllowProviderWrites {
			opts.OperationWriteFamilies = append(opts.OperationWriteFamilies, mcpserver.OperationFamilyProviders)
		}
		if mcpAllowDocumentWrites {
			opts.OperationWriteFamilies = append(opts.OperationWriteFamilies, mcpserver.OperationFamilyDocuments)
		}
		if mcpAllowCardDAVOperationWrites {
			opts.OperationWriteFamilies = append(opts.OperationWriteFamilies, mcpserver.OperationFamilyCardDAV)
		}
		if mcpAllowSettingsWrites {
			opts.OperationWriteFamilies = append(opts.OperationWriteFamilies, mcpserver.OperationFamilySettings)
		}
		if mcpAllowEnrichmentWrites {
			opts.OperationWriteFamilies = append(opts.OperationWriteFamilies, mcpserver.OperationFamilyEnrichment)
		}
		if mcpAllowVisualWrites {
			opts.OperationWriteFamilies = append(opts.OperationWriteFamilies, mcpserver.OperationFamilyVisual)
		}

		if mcpHTTPAddr != "" {
			normalized, err := normalizeMCPHTTPAddr(
				mcpHTTPAddr,
				mcpHTTPAllowInsecure,
				cfg.Server.APIKey != "",
			)
			if err != nil {
				return usageErr(cmd, err)
			}
			return serveMCPHTTPWithOptions(ctx, opts, mcpserver.HTTPOptions{
				Addr:               normalized,
				DiscoveryDirectory: filepath.Join(cfg.HomeDir, "mcp"),
				BackendURL:         info.URL,
				APIKey:             cfg.Server.APIKey,
				AllowWrites:        mcpHTTPAllowWrites,
			})
		}
		return mcpserver.ServeWithOptions(ctx, opts)
	},
}

// savedViewsMinAPISchemaVersion is the first daemon API schema that runs Saved
// Views through POST /api/v1/saved-views/{id}/run.
const savedViewsMinAPISchemaVersion = "2.21.0"
const personCardDAVMinAPISchemaVersion = "2.32.0"
const identityReviewMinAPISchemaVersion = "2.36.0"
const identityScoringMinAPISchemaVersion = "2.36.0"

// personAgendaMinAPISchemaVersion adds live task-backed person agendas.
const personAgendaMinAPISchemaVersion = "2.30.0"

// archiveSQLMinAPISchemaVersion adds SQL confined to archive analytics files.
const archiveSQLMinAPISchemaVersion = "2.31.0"

// Schema 2.28.0 adds independent configured-lane facts to authenticated
// health. Older health responses cannot distinguish text from visual search.
const vectorLaneHealthMinAPISchemaVersion = "2.28.0"

// Schema 2.4.0 added the visual attachment search route.
const visualSearchMinAPISchemaVersion = "2.4.0"

func daemonMCPServeOptions(ctx context.Context, st *daemonclient.Client, state *invocation) mcpserver.ServeOptions {
	if state == nil {
		state = invocationFromContext(ctx)
	}
	var cfg *config.Config
	log := slog.New(slog.DiscardHandler)
	if state != nil {
		cfg = state.cfg
		if state.logger != nil {
			log = state.logger
		}
	}
	engine := daemonclient.NewEngineAdapter(st)
	opts := mcpserver.ServeOptions{
		DelegatedOnly:      st.UsesDelegatedAuthentication(),
		Engine:             engine,
		AttachmentReader:   st,
		ManifestSaver:      daemonMCPManifestSaver{client: st},
		DocumentSearcher:   st,
		PersonFileSearcher: daemonMCPPersonFileSearcher{client: st},
	}
	if cfg != nil {
		opts.AttachmentsDir = cfg.AttachmentsDir()
		opts.DataDir = cfg.Data.DataDir
	}
	capabilities, discoveryErr := st.MCPCapabilities(ctx)
	if discoveryErr != nil {
		log.Warn("operational MCP tools disabled because daemon discovery failed", "error", discoveryErr)
	} else {
		backend := newDaemonMCPOperations(st, capabilities)
		opts.Operations = backend
		opts.OperationCapabilities = backend.capabilities()
		opts.DelegatedOnly = capabilities.Delegated
	}
	if opts.DelegatedOnly {
		return opts
	}
	health, capabilityErr := st.Health(ctx)
	var schemaVersion string
	if health != nil && health.APISchemaVersion != nil {
		schemaVersion = *health.APISchemaVersion
	}
	if capabilityErr == nil && health != nil && health.Vector != nil &&
		daemonclient.APISchemaVersionAtLeast(schemaVersion, vectorLaneHealthMinAPISchemaVersion) {
		if health.Vector.TextEnabled != nil && *health.Vector.TextEnabled {
			opts.HybridSearcher = daemonMCPHybridSearcher{client: st}
			opts.SimilarSearcher = daemonMCPSimilarSearcher{client: st}
		}
		if health.Vector.VisualEnabled != nil && *health.Vector.VisualEnabled &&
			daemonclient.APISchemaVersionAtLeast(schemaVersion, visualSearchMinAPISchemaVersion) {
			opts.VisualSearcher = daemonMCPVisualSearcher{client: st}
		}
	}
	if capabilityErr != nil {
		log.Warn("people tools disabled because the daemon capability probe failed", "error", capabilityErr)
	} else if daemonclient.APISchemaVersionAtLeast(schemaVersion, peopleMinAPISchemaVersion) {
		people := daemonclient.NewPeopleBrowser(engine)
		if daemonclient.APISchemaVersionAtLeast(schemaVersion, directoryPeopleMinAPISchemaVersion) {
			opts.DirectoryBackend = people
		}
		opts.PeopleBackend = people
		if supportsNamedPromotion(capabilities) {
			opts.PeopleBackend = daemonMCPNamedPeopleBrowser{PeopleBrowser: people, client: st}
		}
	}
	// The daemon executes Saved Views itself, so the tools need a daemon that
	// serves the run endpoint; an older daemon simply omits them.
	if capabilityErr != nil {
		log.Warn("Saved View tools disabled because the daemon capability probe failed", "error", capabilityErr)
	} else if daemonclient.APISchemaVersionAtLeast(schemaVersion, savedViewsMinAPISchemaVersion) {
		opts.SavedViews = st
	}
	if capabilityErr != nil {
		log.Warn("meeting tools disabled because the daemon capability probe failed", "error", capabilityErr)
	} else if daemonclient.APISchemaVersionAtLeast(schemaVersion, meetingsMinAPISchemaVersion) {
		opts.Meetings = st
	}
	if capabilityErr == nil && daemonclient.APISchemaVersionAtLeast(schemaVersion, personAgendaMinAPISchemaVersion) {
		opts.PersonAgendaBackend = st
	}
	if capabilityErr == nil && daemonclient.APISchemaVersionAtLeast(schemaVersion, archiveSQLMinAPISchemaVersion) &&
		(health.AnalyticsEngine == nil || *health.AnalyticsEngine != api.AnalyticsModePostgres) {
		opts.ArchiveSQLQuerier = engine
	}
	if capabilityErr == nil && daemonclient.APISchemaVersionAtLeast(schemaVersion, identityReviewMinAPISchemaVersion) {
		opts.IdentityReview = st
	}
	if capabilityErr == nil && daemonclient.APISchemaVersionAtLeast(schemaVersion, personCardDAVMinAPISchemaVersion) {
		opts.PersonCardDAV = st
	}
	if capabilityErr == nil && daemonclient.APISchemaVersionAtLeast(schemaVersion, identityScoringMinAPISchemaVersion) {
		opts.IdentityScoring = st
	}

	return opts
}

type daemonMCPPersonFileSearcher struct{ client *daemonclient.Client }

func (s daemonMCPPersonFileSearcher) SearchPersonFiles(
	ctx context.Context,
	request mcpserver.PersonFileSearchRequest,
) (generated.PersonFileSearchHTTPResponse, error) {
	return s.client.SearchPersonFiles(ctx, daemonclient.PersonFileSearchOptions{
		PersonID: request.PersonID, Directions: request.Directions,
		After: request.After, Before: request.Before, Filename: request.Filename,
		MIMEFamilies: request.MIMEFamilies, Limit: request.Limit, Cursor: request.Cursor,
	})
}

type daemonMCPVisualSearcher struct{ client *daemonclient.Client }

func (s daemonMCPVisualSearcher) SearchVisualAttachments(ctx context.Context, request mcpserver.VisualSearchRequest) (*visual.SearchResponse, error) {
	return s.client.SearchVisualAttachmentsFiltered(ctx, daemonclient.VisualSearchOptions{
		Text: request.Text, Image: request.Image, Limit: request.Limit, Cursor: request.Cursor,
		SenderPersonID: request.SenderPersonID, SourceID: request.SourceID, MessageID: request.MessageID,
		PersonID: request.PersonID, ParticipantID: request.ParticipantID, Directions: request.Directions,
		Filename: request.Filename, MIMEPrefix: request.MIMEPrefix, After: request.After, Before: request.Before,
	})
}

type daemonMCPHybridSearcher struct {
	client *daemonclient.Client
}

type daemonMCPManifestSaver struct {
	client *daemonclient.Client
}

func (s daemonMCPManifestSaver) SaveManifest(ctx context.Context, manifest *deletion.Manifest) error {
	_, err := s.client.CreateCLIDeletionManifest(ctx, manifest)
	return err
}

func (s daemonMCPHybridSearcher) SearchHybrid(
	ctx context.Context,
	req mcpserver.HybridSearchRequest,
) (*mcpserver.HybridSearchResult, error) {
	resp, err := s.client.GetCLIHybridSearch(ctx, daemonclient.CLIHybridSearchRequest{
		Query:          req.Query,
		Account:        req.Account,
		Mode:           req.Mode,
		Limit:          req.Limit,
		Offset:         req.Offset,
		IncludeMatches: req.IncludeMatches,
		MinScore:       req.MinScore,
	})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return &mcpserver.HybridSearchResult{}, nil
	}

	hits := make([]mcpserver.HybridSearchHit, len(resp.Results))
	for i, hit := range resp.Results {
		out := mcpserver.HybridSearchHit{
			ID:               hit.ID,
			RRFScore:         hit.RRFScore,
			BM25Score:        hit.BM25Score,
			VectorScore:      hit.VectorScore,
			SubjectBoosted:   hit.SubjectBoosted,
			MatchesTruncated: hit.MatchesTruncated,
		}
		if len(hit.Matches) > 0 {
			out.Matches = make([]mcpserver.HybridSearchMatch, len(hit.Matches))
			for j, match := range hit.Matches {
				out.Matches[j] = mcpserver.HybridSearchMatch{
					CharOffset: match.CharOffset,
					Snippet:    match.Snippet,
					Line:       match.Line,
					Score:      match.Score,
				}
			}
		}
		hits[i] = out
	}
	return &mcpserver.HybridSearchResult{
		Hits:          hits,
		PoolSaturated: resp.PoolSaturated,
		Accelerator:   resp.Accelerator,
		HasMore:       resp.HasMore,
		TookMS:        resp.TookMS,
		Timings: mcpserver.HybridSearchTimings{
			QueryEmbeddingMS: resp.Timings.QueryEmbeddingMS,
			RetrievalMS:      resp.Timings.RetrievalMS,
			HydrationMS:      resp.Timings.HydrationMS,
		},
		Generation: mcpserver.HybridGeneration{
			ID:          resp.Generation.ID,
			Model:       resp.Generation.Model,
			Dimension:   resp.Generation.Dimension,
			Fingerprint: resp.Generation.Fingerprint,
			State:       resp.Generation.State,
		},
	}, nil
}

type daemonMCPSimilarSearcher struct {
	client *daemonclient.Client
}

func (s daemonMCPSimilarSearcher) FindSimilar(
	ctx context.Context,
	req mcpserver.SimilarSearchRequest,
) (*mcpserver.SimilarSearchResult, error) {
	resp, err := s.client.FindSimilarMessages(ctx, daemonclient.SimilarSearchRequest{
		MessageID:     req.MessageID,
		Limit:         req.Limit,
		Account:       req.Account,
		MessageType:   req.MessageType,
		After:         req.After,
		Before:        req.Before,
		HasAttachment: req.HasAttachment,
	})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return &mcpserver.SimilarSearchResult{SeedMessageID: req.MessageID}, nil
	}
	return &mcpserver.SimilarSearchResult{
		SeedMessageID: resp.SeedMessageID,
		Generation: mcpserver.HybridGeneration{
			ID:          resp.Generation.ID,
			Model:       resp.Generation.Model,
			Dimension:   resp.Generation.Dimension,
			Fingerprint: resp.Generation.Fingerprint,
			State:       resp.Generation.State,
		},
		Messages: resp.Messages,
	}, nil
}

func init() {
	mcpCmd.AddCommand(newMCPStatusCommand())
	rootCmd.AddCommand(mcpCmd)
	mcpCmd.Flags().BoolVar(&mcpForceSQL, "force-sql", false, "Deprecated in 0.17.0: set [analytics].engine = \"sql\" in config.toml")
	mcpCmd.Flags().BoolVar(&mcpNoSQLiteScanner, "no-sqlite-scanner", false, "Deprecated in 0.17.0: cache engine selection is daemon-managed")
	mcpCmd.Flags().StringVar(&mcpHTTPAddr, "http", "",
		"Serve over StreamableHTTP on this address (e.g. 127.0.0.1:8080) "+
			"instead of stdio. Bare port forms (':8080', '8080') bind to "+
			"loopback only; non-loopback hosts require [server].api_key or "+
			"--http-allow-insecure.")
	mcpCmd.Flags().BoolVar(&mcpHTTPAllowInsecure, "http-allow-insecure", false,
		"Allow --http to bind a non-loopback address without [server].api_key. "+
			"Any configured key still requires bearer authentication. Without a "+
			"key, any reachable client can read your archive; only set this behind "+
			"a trusted network boundary or authenticating reverse proxy.")
	mcpCmd.Flags().BoolVar(&mcpHTTPAllowWrites, "http-allow-writes", false,
		"Expose write-class MCP tools over HTTP. This permits attachment exports, "+
			"deletion manifests, Saved View management, and profile writes separately enabled with "+
			"--allow-profile-writes, identity decisions, identity scoring, person merges, and CardDAV writes enabled "+
			"with their separate opt-ins; enable it only for trusted, authenticated clients.")
	mcpCmd.Flags().BoolVar(&mcpAllowProfileWrites, "allow-profile-writes", false,
		"Expose person promotion and private Notes writes. Model tool calls "+
			"can persist profile data, so enable this only for sessions where the user "+
			"has explicitly authorized profile writes.")
	mcpCmd.Flags().BoolVar(&mcpAllowIdentityDecisions, "allow-identity-decisions", false,
		"Expose identity match accept/reject tools. Each call requires MCP client confirmation; the client must obtain user approval.")
	mcpCmd.Flags().BoolVar(&mcpAllowIdentityScoring, "allow-identity-scoring", false,
		"Expose manual identity scoring that sends evidence to the fixed Jev provider. Each call requires MCP client confirmation; the client must obtain user approval.")
	mcpCmd.Flags().BoolVar(&mcpAllowPersonMerges, "allow-person-merges", false,
		"Expose local person merge tools. Each call requires MCP client confirmation; the client must obtain user approval.")
	mcpCmd.Flags().BoolVar(&mcpAllowCardDAVWrites, "allow-carddav-writes", false,
		"Expose CardDAV publication and sync tools. Each call requires MCP client confirmation; the client must obtain user approval.")
	mcpCmd.Flags().BoolVar(&mcpAllowSourceWrites, "allow-source-writes", false,
		"Expose source synchronization and source policy writes. Each operation requires client confirmation; HTTP also requires --http-allow-writes.")
	mcpCmd.Flags().BoolVar(&mcpAllowProviderWrites, "allow-provider-writes", false, "Enable approved provider policy changes and synthetic checks; HTTP also requires --http-allow-writes")
	mcpCmd.Flags().BoolVar(&mcpAllowDocumentWrites, "allow-document-writes", false, "Enable approved exact-policy document consent and indexing; requires --document-capabilities and local daemon; HTTP also requires --http-allow-writes")
	mcpCmd.Flags().BoolVar(&mcpAllowCardDAVOperationWrites, "allow-carddav-operation-writes", false, "Enable approved CardDAV sync, book roles, conflict resolution and unpublication; HTTP also requires --http-allow-writes")
	mcpCmd.Flags().BoolVar(&mcpAllowSettingsWrites, "allow-settings-writes", false, "Enable approved fixed operational settings changes; HTTP also requires --http-allow-writes")
	mcpCmd.Flags().BoolVar(&mcpAllowEnrichmentWrites, "allow-enrichment-writes", false, "Enable approved existing enrichment policy and global controls changes; HTTP also requires --http-allow-writes")
	mcpCmd.Flags().BoolVar(&mcpAllowInferenceRuns, "allow-inference-runs", false, "Enable approved bounded people sweep and brief generation using existing provider consent; HTTP also requires --http-allow-writes")
	mcpCmd.Flags().BoolVar(&mcpAllowVisualWrites, "allow-visual-writes", false, "Enable approved exact-policy visual build, resume, retry and retirement; HTTP also requires --http-allow-writes")
	mcpCmd.Flags().StringVar(&mcpDocumentCapabilities, "document-capabilities", "", "Operator-selected authenticated manifest on the local daemon host; never accepted from an MCP client")
	mcpCmd.Flags().BoolVar(&mcpAllowDraftWrites, "allow-draft-writes", false, "Enable approved draft creation, editing, deletion and recovery; HTTP also requires --http-allow-writes")
	_ = mcpCmd.Flags().MarkDeprecated("force-sql", "deprecated in 0.17.0; set [analytics].engine = \"sql\" in config.toml")
	_ = mcpCmd.Flags().MarkDeprecated("no-sqlite-scanner", "deprecated in 0.17.0; cache engine selection is daemon-managed; use [analytics].engine = \"sql\" for live SQL")
	_ = mcpCmd.Flags().MarkHidden("force-sql")
	_ = mcpCmd.Flags().MarkHidden("no-sqlite-scanner")
}

// normalizeMCPHTTPAddr canonicalises a --http argument and rejects values
// that would expose an unauthenticated MCP server on a non-loopback interface
// unless the user has configured authentication or explicitly opted in.
//
// Forms accepted:
//   - "8080"            → "127.0.0.1:8080" (loopback)
//   - ":8080"           → "127.0.0.1:8080" (loopback; Go's default would be
//     all-interfaces, which is the footgun this guards against)
//   - "127.0.0.1:8080"  → unchanged (loopback, allowed)
//   - "[::1]:8080"      → unchanged (loopback, allowed)
//   - "192.168.1.5:8080", "0.0.0.0:8080", "vault.local:8080" → rejected
//     unless [server].api_key or --http-allow-insecure is set
func normalizeMCPHTTPAddr(addr string, allowInsecure, authenticated bool) (string, error) {
	trimmed := strings.TrimSpace(addr)
	if trimmed == "" {
		return "", errors.New("--http requires an address")
	}

	// Bare port: "8080" or ":8080".
	if !strings.Contains(trimmed, ":") {
		if _, convErr := strconv.Atoi(trimmed); convErr == nil {
			return "127.0.0.1:" + trimmed, nil
		}
		return "", fmt.Errorf(
			"--http %q: not a port and not host:port", trimmed)
	}
	if strings.HasPrefix(trimmed, ":") {
		return "127.0.0.1" + trimmed, nil
	}

	host, _, splitErr := net.SplitHostPort(trimmed)
	if splitErr != nil {
		return "", fmt.Errorf("--http %q: %w", trimmed, splitErr)
	}

	if isLoopbackHost(host) {
		return trimmed, nil
	}
	if !authenticated && !allowInsecure {
		return "", fmt.Errorf(
			"--http %q: refusing to bind a non-loopback address without "+
				"[server].api_key or --http-allow-insecure (configure an API key "+
				"for bearer authentication, or only opt into unauthenticated "+
				"access behind a trusted network boundary)", trimmed)
	}
	return trimmed, nil
}

// isLoopbackHost reports whether host resolves to a loopback address.
// Empty host is NOT treated as loopback: net.Listen on a host:port pair
// with an empty host binds to all interfaces, which is the exact footgun
// this guard exists to catch (e.g. "[]:8080" passes net.SplitHostPort
// with an empty host but binds to all-interfaces).
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
