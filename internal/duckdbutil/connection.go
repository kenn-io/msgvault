// Package duckdbutil owns resource-bounded DuckDB connection setup.
package duckdbutil

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	_ "github.com/duckdb/duckdb-go/v2" // Register the DuckDB database/sql driver.
)

var duckDBSizePattern = regexp.MustCompile(`(?i)^[1-9][0-9]*(B|KB|MB|GB|TB|KIB|MIB|GIB|TIB)$`)

// Policy defines the complete resource policy for one DuckDB connection.
type Policy struct {
	MemoryLimit          string
	Threads              int
	TempDirectory        string
	MaxTempDirectorySize string
}

// BuilderOverrides contains optional cache-builder policy values. Empty size
// values and zero threads retain BuilderPolicy defaults.
type BuilderOverrides struct {
	MemoryLimit          string
	Threads              int
	MaxTempDirectorySize string
}

// InteractivePolicy returns the bounded policy for daemon-side queries.
func InteractivePolicy(tempDirectory string) Policy {
	return Policy{
		MemoryLimit:          "512MB",
		Threads:              min(runtime.GOMAXPROCS(0), 4),
		TempDirectory:        tempDirectory,
		MaxTempDirectorySize: "2GB",
	}
}

// BuilderPolicy returns the bounded policy for cache derivation processes.
func BuilderPolicy(tempDirectory string) Policy {
	return Policy{
		MemoryLimit:          "2GB",
		Threads:              min(runtime.GOMAXPROCS(0), 2),
		TempDirectory:        tempDirectory,
		MaxTempDirectorySize: "32GB",
	}
}

// BuilderPolicyWithOverrides applies non-zero overrides to BuilderPolicy.
func BuilderPolicyWithOverrides(tempDirectory string, overrides BuilderOverrides) Policy {
	policy := BuilderPolicy(tempDirectory)
	if overrides.MemoryLimit != "" {
		policy.MemoryLimit = overrides.MemoryLimit
	}
	if overrides.Threads != 0 {
		policy.Threads = overrides.Threads
	}
	if overrides.MaxTempDirectorySize != "" {
		policy.MaxTempDirectorySize = overrides.MaxTempDirectorySize
	}
	return policy
}

// Open creates one in-memory DuckDB connection and applies policy before any
// analytical work can run.
func Open(ctx context.Context, policy Policy) (*sql.DB, error) {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, fmt.Errorf("open duckdb: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := Apply(ctx, db, policy); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// Apply validates and installs a complete resource policy on db.
func Apply(ctx context.Context, db *sql.DB, policy Policy) error {
	if db == nil {
		return errors.New("apply duckdb policy: nil database")
	}
	if policy.Threads <= 0 {
		return errors.New("apply duckdb policy: threads must be positive")
	}
	if !ValidSize(policy.MemoryLimit) {
		return fmt.Errorf("apply duckdb policy: invalid memory limit %q", policy.MemoryLimit)
	}
	if !ValidSize(policy.MaxTempDirectorySize) {
		return fmt.Errorf("apply duckdb policy: invalid temp-directory limit %q", policy.MaxTempDirectorySize)
	}
	if policy.TempDirectory == "" || !filepath.IsAbs(policy.TempDirectory) {
		return errors.New("apply duckdb policy: temp directory must be absolute")
	}
	if err := os.MkdirAll(policy.TempDirectory, 0o700); err != nil {
		return fmt.Errorf("create duckdb temp directory: %w", err)
	}

	quotedTemp := strings.ReplaceAll(filepath.Clean(policy.TempDirectory), "'", "''")
	settings := []struct {
		name string
		sql  string
	}{
		{name: "memory_limit", sql: "SET memory_limit = '" + policy.MemoryLimit + "'"},
		{name: "threads", sql: fmt.Sprintf("SET threads = %d", policy.Threads)},
		{name: "preserve_insertion_order", sql: "SET preserve_insertion_order = false"},
		{name: "temp_directory", sql: "SET temp_directory = '" + quotedTemp + "'"},
		{name: "max_temp_directory_size", sql: "SET max_temp_directory_size = '" + policy.MaxTempDirectorySize + "'"},
	}
	for _, setting := range settings {
		if _, err := db.ExecContext(ctx, setting.sql); err != nil {
			return fmt.Errorf("set duckdb %s: %w", setting.name, err)
		}
	}
	return nil
}

// ValidSize reports whether value is a positive integer followed by a
// case-insensitive DuckDB byte-size unit.
func ValidSize(value string) bool {
	return duckDBSizePattern.MatchString(value)
}
