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
	"go.kenn.io/msgvault/internal/providercredentials"
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
var mcpAllowKataWrites bool
var mcpAllowCalendarWrites bool
var serveMCPStdioWithOptions = mcpserver.ServeWithOptions
var serveMCPHTTPWithOptions = mcpserver.ServeHTTPWithOptions

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Run MCP server for Claude Desktop integration",
	Long: `Start an MCP (Model Context Protocol) server over stdio.

This allows Claude Desktop (or any MCP client) to query your archive
using tools like search_metadata, search_message_bodies, search_document_attachments, semantic_search_messages, get_message, list_messages, list_thread, export_eml, get_stats,
aggregate, get_person_agenda, list_saved_views, run_saved_view, stage_deletion,
draft_reply, draft_compose, draft_forward, draft_get, draft_edit, draft_delete,
draft_recover, and draft_send_as.
Draft tools create, read, edit, delete, and recover managed drafts through the
daemon. Msgvault never sends. With --agent-url and --agent-token-file, stdio
exposes only delegated draft and calendar tools and the daemon enforces the
agent grant.

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
		if state == nil || (state.cfg == nil && !isAgentMode(state)) {
			return errors.New("configuration is unavailable")
		}
		if isAgentMode(state) && mcpHTTPAddr != "" {
			return usageErr(cmd, errors.New("delegated MCP supports stdio only"))
		}
		cfg := state.cfg
		httpAddr, inboundKey, err := prepareMCPHTTP(cmd, cfg)
		if err != nil {
			return usageErr(cmd, err)
		}
		st, info, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return fmt.Errorf("open daemon: %w", err)
		}
		defer func() { _ = st.Close() }()
		if httpAddr != "" {
			if !cmd.Flags().Changed("http-token-file") && !cmd.Flags().Changed("http-token-env") {
				// Local startup may have created the key. OpenHTTPStore refreshes
				// it after discovering or starting the daemon that owns the archive.
				inboundKey = cfg.Server.AuthenticationKey()
			}
			httpAddr, err = normalizeMCPHTTPAddr(httpAddr, mcpHTTPAllowInsecure, inboundKey != "")
			if err != nil {
				return usageErr(cmd, err)
			}
		}

		// Derive from cmd.Context() so signal handling installed by
		// the cobra root command (SIGINT/SIGTERM → ctx.Done()) reaches
		// the MCP transport and can trigger ServeHTTPWithOptions's
		// graceful shutdown.
		ctx, cancel := context.WithCancel(cmd.Context())
		defer cancel()

		var opts mcpserver.ServeOptions
		if isAgentMode(state) {
			if opts, err = delegatedMCPServeOptions(ctx, st); err != nil {
				return err
			}
		} else {
			opts = daemonMCPServeOptions(ctx, st, state)
		}
		opts.AllowProfileWrites = mcpAllowProfileWrites
		opts.AllowIdentityDecisions = mcpAllowIdentityDecisions
		opts.AllowIdentityScoring = mcpAllowIdentityScoring
		opts.AllowPersonMerges = mcpAllowPersonMerges
		opts.AllowCardDAVWrites = mcpAllowCardDAVWrites
		opts.AllowKataWrites = mcpAllowKataWrites
		opts.AllowCalendarWrites = mcpAllowCalendarWrites

		if httpAddr != "" {
			return serveMCPHTTPWithOptions(ctx, opts, mcpserver.HTTPOptions{
				Addr:               httpAddr,
				DiscoveryDirectory: filepath.Join(cfg.HomeDir, "mcp"),
				BackendURL:         info.URL,
				APIKey:             inboundKey,
				AllowWrites:        mcpHTTPAllowWrites,
			})
		}
		return serveMCPStdioWithOptions(ctx, opts)
	},
}

// prepareMCPHTTP validates inbound sources and the address without creating keys.
// Explicit inbound sources leave the daemon's unused server credential alone.
func prepareMCPHTTP(cmd *cobra.Command, cfg *config.Config) (string, string, error) {
	fileSet := cmd.Flags().Changed("http-token-file")
	envSet := cmd.Flags().Changed("http-token-env")
	if mcpHTTPAddr == "" {
		if fileSet || envSet || cmd.Flags().Changed("http") {
			return "", "", errors.New("HTTP token flags require --http with an address")
		}
		return "", "", nil
	}
	var key string
	var err error
	var deferKeyCheck bool
	switch {
	case fileSet:
		path, _ := cmd.Flags().GetString("http-token-file")
		if path == "" {
			return "", "", errors.New("--http-token-file must not be empty")
		}
		key, err = providercredentials.ReadSecretFile(path)
	case envSet:
		name, _ := cmd.Flags().GetString("http-token-env")
		if name == "" {
			return "", "", errors.New("--http-token-env must not be empty")
		}
		key, err = providercredentials.ResolveSecret("", "", name)
	default:
		err = cfg.ResolveServerKey()
		key = cfg.Server.AuthenticationKey()
		// A local daemon may create the default key during startup. Enforce
		// the inbound key requirement after OpenHTTPStore has resolved it.
		deferKeyCheck = !isRemoteModeFor(invocationFromCommand(cmd))
	}
	if err != nil {
		return "", "", fmt.Errorf("MCP inbound credential: %w", err)
	}
	address, err := normalizeMCPHTTPAddr(mcpHTTPAddr, mcpHTTPAllowInsecure, key != "" || deferKeyCheck)
	return address, key, err
}

// savedViewsMinAPISchemaVersion is the first daemon API schema that runs Saved
// Views through POST /api/v1/saved-views/{id}/run.
const savedViewsMinAPISchemaVersion = "2.21.0"
const personCardDAVMinAPISchemaVersion = "2.32.0"
const identityReviewMinAPISchemaVersion = "3.0.0"
const identityScoringMinAPISchemaVersion = "3.0.0"
const draftsMinAPISchemaVersion = "3.0.0"

// delegatedMCPServeOptions offers only the tools an agent grant can use.
// Draft tools are the floor, so an older daemon fails instead of serving an empty msgvault.
func delegatedMCPServeOptions(ctx context.Context, st *daemonclient.Client) (mcpserver.ServeOptions, error) {
	health, err := st.Health(ctx)
	if err != nil {
		return mcpserver.ServeOptions{}, fmt.Errorf("check daemon compatibility: %w", err)
	}
	var schemaVersion string
	if health != nil && health.APISchemaVersion != nil {
		schemaVersion = *health.APISchemaVersion
	}
	if !daemonclient.APISchemaVersionAtLeast(schemaVersion, draftsMinAPISchemaVersion) {
		return mcpserver.ServeOptions{}, fmt.Errorf("MCP draft tools require daemon API schema %s or newer (daemon reports %q); upgrade the daemon", draftsMinAPISchemaVersion, schemaVersion)
	}
	opts := mcpserver.ServeOptions{
		Drafts: daemonMCPDraftRunner{client: st}, DraftCommands: mcpDraftCommands(true), DelegatedOnly: true,
	}
	if daemonclient.APISchemaVersionAtLeast(schemaVersion, calendarControlMinAPISchemaVersion) {
		opts.Calendar = st
	}
	return opts, nil
}

// personAgendaMinAPISchemaVersion adds live task-backed person agendas.
const personAgendaMinAPISchemaVersion = "2.30.0"

// archiveSQLMinAPISchemaVersion adds SQL confined to archive analytics files.
const archiveSQLMinAPISchemaVersion = "2.31.0"

// calendarControlMinAPISchemaVersion adds delegated Calendar tools.
const calendarControlMinAPISchemaVersion = "3.1.0"

// kataIssuesMinAPISchemaVersion adds Kata issues that quote archive evidence.
const kataIssuesMinAPISchemaVersion = "3.3.0"

// kataLookupMinAPISchemaVersion adds finding the Kata issues that cite a source.
const kataLookupMinAPISchemaVersion = "3.4.0"

// kataContextMinAPISchemaVersion adds reading back the passages a Kata issue cites.
const kataContextMinAPISchemaVersion = "3.5.0"

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
		Engine:             engine,
		AttachmentReader:   st,
		ManifestSaver:      daemonMCPManifestSaver{client: st},
		DocumentSearcher:   st,
		PersonFileSearcher: daemonMCPPersonFileSearcher{client: st},
		Drafts:             daemonMCPDraftRunner{client: st},
	}
	if cfg != nil {
		opts.AttachmentsDir = cfg.AttachmentsDir()
		opts.DataDir = cfg.Data.DataDir
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
	if capabilityErr == nil && daemonclient.APISchemaVersionAtLeast(schemaVersion, calendarControlMinAPISchemaVersion) {
		opts.Calendar = st
	}
	if capabilityErr == nil && daemonclient.APISchemaVersionAtLeast(schemaVersion, personAgendaMinAPISchemaVersion) {
		opts.PersonAgendaBackend = st
	}
	if capabilityErr == nil && daemonclient.APISchemaVersionAtLeast(schemaVersion, kataIssuesMinAPISchemaVersion) {
		opts.Kata = st
		opts.KataLookup = daemonclient.APISchemaVersionAtLeast(schemaVersion, kataLookupMinAPISchemaVersion)
		opts.KataContext = daemonclient.APISchemaVersionAtLeast(schemaVersion, kataContextMinAPISchemaVersion)
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
	if capabilityErr == nil && daemonclient.APISchemaVersionAtLeast(schemaVersion, draftsMinAPISchemaVersion) {
		opts.DraftCommands = mcpDraftCommands(false)
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
			"loopback only; non-loopback hosts require an inbound key or "+
			"--http-allow-insecure.")
	mcpCmd.Flags().String("http-token-file", "", "Read an independent inbound bearer key from an owner-only file (requires --http)")
	mcpCmd.Flags().String("http-token-env", "", "Name the environment variable holding an inbound bearer key (requires --http; file takes priority)")
	mcpCmd.Flags().BoolVar(&mcpHTTPAllowInsecure, "http-allow-insecure", false,
		"Allow --http to bind a non-loopback address without an inbound key. "+
			"Any configured key still requires bearer authentication. Without a "+
			"key, any reachable client can read your archive; only set this behind "+
			"a trusted network boundary or authenticating reverse proxy.")
	mcpCmd.Flags().BoolVar(&mcpHTTPAllowWrites, "http-allow-writes", false,
		"Expose write-class MCP tools over HTTP. This permits attachment exports, "+
			"deletion manifests, Saved View management, managed draft writes, and profile writes separately enabled with "+
			"--allow-profile-writes, identity decisions, identity scoring, person merges, CardDAV writes, calendar writes, and Kata issue writes enabled "+
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
	mcpCmd.Flags().BoolVar(&mcpAllowKataWrites, "allow-kata-writes", false,
		"Expose Kata issue creation and evidence linking tools. Archive text is untrusted input; enable only when the user explicitly authorizes Kata issue writes.")
	mcpCmd.Flags().BoolVar(&mcpAllowCardDAVWrites, "allow-carddav-writes", false,
		"Expose CardDAV publication and sync tools. Each call requires MCP client confirmation; the client must obtain user approval.")
	mcpCmd.Flags().BoolVar(&mcpAllowCalendarWrites, "allow-calendar-writes", false,
		"Expose calendar event mutation tools. Calendar event text is untrusted input; enable only when the user explicitly authorizes calendar writes.")
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
				"an inbound key or --http-allow-insecure (configure an API key "+
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
