package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/deletion"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
)

var mcpForceSQL bool
var mcpNoSQLiteScanner bool
var mcpHTTPAddr string
var mcpHTTPAllowInsecure bool

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Run MCP server for Claude Desktop integration",
	Long: `Start an MCP (Model Context Protocol) server over stdio.

This allows Claude Desktop (or any MCP client) to query your email archive
using tools like search_metadata, search_message_bodies, semantic_search_messages, get_message, list_messages, get_stats,
aggregate, and stage_deletion.

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
		st, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return fmt.Errorf("open daemon: %w", err)
		}
		defer func() { _ = st.Close() }()

		// Derive from cmd.Context() so signal handling installed by
		// the cobra root command (SIGINT/SIGTERM → ctx.Done()) reaches
		// the MCP transport and can trigger ServeHTTPWithOptions's
		// graceful shutdown.
		ctx, cancel := context.WithCancel(cmd.Context())
		defer cancel()

		opts, err := daemonMCPServeOptions(ctx, st)
		if err != nil {
			return err
		}

		if mcpHTTPAddr != "" {
			normalized, err := normalizeMCPHTTPAddr(mcpHTTPAddr, mcpHTTPAllowInsecure)
			if err != nil {
				return usageErr(cmd, err)
			}
			return mcpserver.ServeHTTPWithOptions(ctx, opts, normalized)
		}
		return mcpserver.ServeWithOptions(ctx, opts)
	},
}

func daemonMCPServeOptions(ctx context.Context, st *daemonclient.Client) (mcpserver.ServeOptions, error) {
	opts := mcpserver.ServeOptions{
		Engine:           daemonclient.NewEngineAdapter(st),
		AttachmentsDir:   cfg.AttachmentsDir(),
		AttachmentReader: st,
		ManifestSaver:    daemonMCPManifestSaver{client: st},
		DataDir:          cfg.Data.DataDir,
	}

	vectorAvailable, err := st.VectorSearchAvailable(ctx)
	if err != nil {
		return mcpserver.ServeOptions{}, fmt.Errorf("check daemon vector search: %w", err)
	}
	if vectorAvailable {
		opts.HybridSearcher = daemonMCPHybridSearcher{client: st}
		opts.SimilarSearcher = daemonMCPSimilarSearcher{client: st}
	}
	return opts, nil
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
		HasMore:       resp.HasMore,
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
	rootCmd.AddCommand(mcpCmd)
	mcpCmd.Flags().BoolVar(&mcpForceSQL, "force-sql", false, "Deprecated in 0.17.0: set [analytics].engine = \"sql\" in config.toml")
	mcpCmd.Flags().BoolVar(&mcpNoSQLiteScanner, "no-sqlite-scanner", false, "Deprecated in 0.17.0: cache engine selection is daemon-managed")
	mcpCmd.Flags().StringVar(&mcpHTTPAddr, "http", "",
		"Serve over StreamableHTTP on this address (e.g. 127.0.0.1:8080) "+
			"instead of stdio. Bare port forms (':8080', '8080') bind to "+
			"loopback only; non-loopback hosts require --http-allow-insecure.")
	mcpCmd.Flags().BoolVar(&mcpHTTPAllowInsecure, "http-allow-insecure", false,
		"Allow --http to bind a non-loopback address. The MCP server has no "+
			"built-in authentication, so any reachable client can read your "+
			"archive. Only set this on trusted networks (Tailscale, "+
			"VPN-only) or behind an authenticating reverse proxy.")
	_ = mcpCmd.Flags().MarkDeprecated("force-sql", "deprecated in 0.17.0; set [analytics].engine = \"sql\" in config.toml")
	_ = mcpCmd.Flags().MarkDeprecated("no-sqlite-scanner", "deprecated in 0.17.0; cache engine selection is daemon-managed; use [analytics].engine = \"sql\" for live SQL")
	_ = mcpCmd.Flags().MarkHidden("force-sql")
	_ = mcpCmd.Flags().MarkHidden("no-sqlite-scanner")
}

// normalizeMCPHTTPAddr canonicalises a --http argument and rejects values
// that would expose the unauthenticated MCP server on a non-loopback
// interface unless the user has explicitly opted in.
//
// Forms accepted:
//   - "8080"            → "127.0.0.1:8080" (loopback)
//   - ":8080"           → "127.0.0.1:8080" (loopback; Go's default would be
//     all-interfaces, which is the footgun this guards against)
//   - "127.0.0.1:8080"  → unchanged (loopback, allowed)
//   - "[::1]:8080"      → unchanged (loopback, allowed)
//   - "192.168.1.5:8080", "0.0.0.0:8080", "vault.local:8080" → rejected
//     unless --http-allow-insecure is set
func normalizeMCPHTTPAddr(addr string, allowInsecure bool) (string, error) {
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
	if !allowInsecure {
		return "", fmt.Errorf(
			"--http %q: refusing to bind a non-loopback address without "+
				"--http-allow-insecure (the MCP server has no built-in "+
				"authentication; only opt in on trusted networks or "+
				"behind an authenticating reverse proxy)", trimmed)
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
