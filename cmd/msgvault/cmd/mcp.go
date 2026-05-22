package cmd

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
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
using tools like search_messages, get_message, list_messages, get_stats,
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
		dbPath := cfg.DatabaseDSN()

		// Open read-only: MCP is a query-only workload. This avoids
		// SQLite write-lock contention when multiple MCP processes
		// (one per Claude Code session) access the same database.
		// Schema migrations and FTS backfill are write operations
		// handled by init-db / sync / tui — not by MCP.
		s, err := store.OpenReadOnly(dbPath)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer func() { _ = s.Close() }()

		if stale, col, err := s.SchemaStale(); err != nil {
			return fmt.Errorf("check schema: %w", err)
		} else if stale {
			return fmt.Errorf(
				"database schema is outdated (missing %s); "+
					"run 'msgvault init-db' to update", col)
		}

		if s.FTS5Available() && s.NeedsFTSBackfill() {
			fmt.Fprintf(os.Stderr,
				"Warning: full-text search index needs populating; "+
					"body-text search will return incomplete results "+
					"until 'msgvault tui' or 'msgvault search' is run\n")
		}

		var engine query.Engine
		analyticsDir := cfg.AnalyticsDir()

		if !mcpForceSQL && query.HasCompleteParquetData(analyticsDir) {
			var duckOpts query.DuckDBOptions
			if mcpNoSQLiteScanner {
				duckOpts.DisableSQLiteScanner = true
			}
			duckEngine, err := query.NewDuckDBEngine(analyticsDir, dbPath, s.DB(), duckOpts)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: Failed to open Parquet engine: %v\n", err)
				fmt.Fprintf(os.Stderr, "Falling back to SQLite\n")
				engine = query.NewSQLiteEngine(s.DB())
			} else {
				engine = duckEngine
				defer func() { _ = duckEngine.Close() }()
			}
		} else {
			engine = query.NewSQLiteEngine(s.DB())
		}

		// Derive from cmd.Context() so signal handling installed by
		// the cobra root command (SIGINT/SIGTERM → ctx.Done()) reaches
		// the MCP transport and can trigger ServeHTTPWithOptions's
		// graceful shutdown.
		ctx, cancel := context.WithCancel(cmd.Context())
		defer cancel()

		// Build optional vector-search components. MCP runs as a
		// query-only server, so the worker and enqueuer fields go
		// unused — only Backend, HybridEngine, and VectorCfg reach
		// the MCP layer.
		vf, err := setupVectorFeatures(ctx, s.DB(), dbPath)
		if err != nil {
			return fmt.Errorf("vector features: %w", err)
		}
		defer func() {
			if vf != nil && vf.Close != nil {
				if closeErr := vf.Close(); closeErr != nil {
					logger.Warn("closing vectors.db failed", "error", closeErr)
				}
			}
		}()

		opts := mcpserver.ServeOptions{
			Engine:         engine,
			AttachmentsDir: cfg.AttachmentsDir(),
			DataDir:        cfg.Data.DataDir,
		}
		if vf != nil {
			opts.HybridEngine = vf.HybridEngine
			opts.Backend = vf.Backend
			opts.VectorCfg = vf.Cfg
		}

		if mcpHTTPAddr != "" {
			normalized, err := normalizeMCPHTTPAddr(mcpHTTPAddr, mcpHTTPAllowInsecure)
			if err != nil {
				return err
			}
			return mcpserver.ServeHTTPWithOptions(ctx, opts, normalized)
		}
		return mcpserver.ServeWithOptions(ctx, opts)
	},
}

func init() {
	rootCmd.AddCommand(mcpCmd)
	mcpCmd.Flags().BoolVar(&mcpForceSQL, "force-sql", false, "Force SQLite queries instead of Parquet")
	mcpCmd.Flags().BoolVar(&mcpNoSQLiteScanner, "no-sqlite-scanner", false, "Disable DuckDB sqlite_scanner extension (use direct SQLite fallback)")
	mcpCmd.Flags().StringVar(&mcpHTTPAddr, "http", "",
		"Serve over StreamableHTTP on this address (e.g. 127.0.0.1:8080) "+
			"instead of stdio. Bare port forms (':8080', '8080') bind to "+
			"loopback only; non-loopback hosts require --http-allow-insecure.")
	mcpCmd.Flags().BoolVar(&mcpHTTPAllowInsecure, "http-allow-insecure", false,
		"Allow --http to bind a non-loopback address. The MCP server has no "+
			"built-in authentication, so any reachable client can read your "+
			"archive. Only set this on trusted networks (Tailscale, "+
			"VPN-only) or behind an authenticating reverse proxy.")
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
		return "", fmt.Errorf("--http requires an address")
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
