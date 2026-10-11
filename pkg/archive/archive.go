// Package archive provides Go access to msgvault storage, synchronization and reads.
// It does not discover local configuration, credentials, or data directories.
package archive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

// PostgreSQL selects a database and optionally a schema. Empty Schema preserves
// the connection's search path. URL accepts a PostgreSQL URL or libpq keyword
// DSN. It may carry credentials and must not be logged.
//
// Each concurrent source sync or purge holds one connection for its session
// advisory lock and needs another for its work. MaxOpenConnections 0 keeps
// msgvault's default pool of 25 connections. Otherwise set it to at least the
// number of concurrent syncs and purges plus one; Open rejects 1 and negative
// values. Transaction poolers are not supported.
type PostgreSQL struct {
	URL                string
	Schema             string
	MaxOpenConnections int
}

func (p PostgreSQL) connectionURL() (string, error) {
	if strings.TrimSpace(p.URL) == "" {
		return "", errors.New("archive requires an explicit PostgreSQL connection")
	}
	parsed, err := pgx.ParseConfig(p.URL)
	if err != nil {
		return "", errors.New("invalid archive PostgreSQL connection")
	}
	if p.Schema == "" {
		return p.URL, nil
	}
	if parsed.RuntimeParams["options"] != "" {
		return "", errors.New("use PostgreSQL connection parameters instead of options when selecting a schema")
	}
	if !store.IsPostgresURL(p.URL) {
		quoted := strings.NewReplacer(`\`, `\\`, "'", `\'`).Replace(pgx.Identifier{p.Schema}.Sanitize() + ",pg_catalog")
		return p.URL + " search_path='" + quoted + "'", nil
	}
	u, err := url.Parse(p.URL)
	if err != nil {
		return "", errors.New("invalid archive PostgreSQL URL")
	}
	params := u.Query()
	params.Set("search_path", pgx.Identifier{p.Schema}.Sanitize()+",pg_catalog")
	u.RawQuery = params.Encode()
	return u.String(), nil
}

// Setup creates or upgrades the selected archive schema. Run it separately
// using a schema-owning database role, before starting runtime readers/writers.
// The caller serializes setup with other setup/upgrade processes.
func Setup(ctx context.Context, postgres PostgreSQL) error {
	dsn, err := postgres.connectionURL()
	if err != nil {
		return err
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open archive setup connection: %w", err)
	}
	defer func() { _ = db.Close() }()
	if postgres.Schema != "" {
		// CREATE SCHEMA checks database CREATE privilege even when the schema
		// exists, so a provisioned schema's owner skips the statement.
		var exists bool
		if err := db.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)", postgres.Schema,
		).Scan(&exists); err != nil {
			return fmt.Errorf("check archive schema: %w", err)
		}
		if !exists {
			if _, err := db.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{postgres.Schema}.Sanitize()); err != nil {
				return fmt.Errorf("create archive schema: %w", err)
			}
		}
	}
	st, err := store.OpenPostgresContext(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	return st.InitSchemaContext(ctx)
}

// Archive owns its connection pool.
// Stop and drain calls into it before Close.
type Archive struct {
	store *store.Store
}

// Open connects to a previously set up archive. It runs no DDL and can use a
// runtime role with only USAGE on the schema and DML on its tables/sequences.
func Open(ctx context.Context, postgres PostgreSQL) (*Archive, error) {
	if postgres.MaxOpenConnections < 0 || postgres.MaxOpenConnections == 1 {
		return nil, fmt.Errorf("PostgreSQL MaxOpenConnections %d cannot hold a sync lock and run its work; "+
			"use 0 for the default pool or at least 2", postgres.MaxOpenConnections)
	}
	dsn, err := postgres.connectionURL()
	if err != nil {
		return nil, err
	}
	st, err := store.OpenPostgresContext(ctx, dsn)
	if err != nil {
		return nil, err
	}
	var current string
	err = st.DB().QueryRowContext(ctx, "SELECT current_schema()").Scan(&current)
	if err == nil && postgres.Schema != "" && current != postgres.Schema {
		err = errors.New("archive schema does not exist; run Setup first")
	}
	if err == nil {
		err = st.ValidateSchemaContext(ctx)
	}
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("open initialized archive: %w", err)
	}
	if postgres.MaxOpenConnections != 0 {
		st.DB().SetMaxOpenConns(postgres.MaxOpenConnections)
	}
	return &Archive{store: st}, nil
}

// Close releases the connection pool. Callers stop their sync workers first.
func (a *Archive) Close() error {
	return a.store.Close()
}

// Store returns the archive's store. The Archive owns its connection lifetime.
func (a *Archive) Store() *Store { return a.store }

// QueryEngine returns a query engine for the archive's storage backend.
func (a *Archive) QueryEngine() QueryEngine {
	if a.store.IsPostgreSQL() {
		return query.NewPostgreSQLEngine(a.store.DB())
	}
	return query.NewSQLiteEngine(a.store.DB())
}

// SetupSQLite creates or upgrades an explicitly selected SQLite archive.
// The caller serializes setup with other setup/upgrade processes.
func SetupSQLite(ctx context.Context, path string) error {
	if store.IsPostgresURL(path) {
		return errors.New("SetupSQLite requires a SQLite file path; use Setup for PostgreSQL")
	}
	st, err := store.OpenContext(ctx, path)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	return st.InitSchemaContext(ctx)
}

// OpenSQLite opens a previously initialized SQLite archive without schema DDL.
// SQLite uses msgvault's normal CGO build and native driver. A missing file
// is an fs.ErrNotExist error; Open never creates the file or its directory.
func OpenSQLite(ctx context.Context, path string) (*Archive, error) {
	if store.IsPostgresURL(path) {
		return nil, errors.New("OpenSQLite requires a SQLite file path; use Open for PostgreSQL")
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("open SQLite archive: %w", err)
	}
	st, err := store.OpenContext(ctx, path)
	if err != nil {
		return nil, err
	}
	if err = st.ValidateSchemaContext(ctx); err != nil {
		_ = st.Close()
		return nil, err
	}
	return &Archive{store: st}, nil
}
