package archive_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/archive"
	"go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestPostgreSQLRuntimeAndReader(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dsn := os.Getenv("MSGVAULT_TEST_DB")
	if !store.IsPostgresURL(dsn) {
		t.Skip("requires a disposable PostgreSQL database")
	}
	admin, err := sql.Open("pgx", dsn)
	require.NoError(err)
	t.Cleanup(func() { _ = admin.Close() })
	schema := "archive_" + strings.ToLower(rand.Text())
	otherSchema := "outside_" + strings.ToLower(rand.Text())
	role := "reader_" + strings.ToLower(rand.Text())
	quote := func(name string) string { return pgx.Identifier{name}.Sanitize() }
	t.Cleanup(func() {
		_, err := admin.Exec("DROP SCHEMA IF EXISTS " + quote(schema) + " CASCADE")
		assert.NoError(err)
		_, err = admin.Exec("DROP SCHEMA IF EXISTS " + quote(otherSchema) + " CASCADE")
		assert.NoError(err)
		_, err = admin.Exec("DROP ROLE IF EXISTS " + quote(role))
		assert.NoError(err)
	})
	opts := archive.PostgreSQL{URL: dsn, Schema: schema}
	// Opening runtime must never create a missing archive.
	_, err = archive.Open(t.Context(), opts)
	require.Error(err)
	require.NoError(archive.Setup(t.Context(), opts))

	// Seed the real store; the public reader must use exactly the same archive.
	u, err := url.Parse(dsn)
	require.NoError(err)
	params := u.Query()
	params.Set("search_path", schema)
	u.RawQuery = params.Encode()
	st, err := store.OpenContext(t.Context(), u.String())
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	source, err := st.GetOrCreateSource("slack", "TEXAMPLE:UEXAMPLE")
	require.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "CEXAMPLE", "Announcements")
	require.NoError(err)
	id, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversation, SourceMessageID: "1704110400.000001",
		MessageType: "slack", SentAt: sql.NullTime{Time: time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC), Valid: true},
		Subject: sql.NullString{String: "Launch", Valid: true},
	})
	require.NoError(err)
	const body = "The observatory opens tomorrow."
	require.NoError(st.UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{}))
	require.NoError(st.UpsertFTS(id, "Launch", body, "", "", ""))

	_, err = admin.Exec("CREATE SCHEMA " + quote(otherSchema))
	require.NoError(err)
	_, err = admin.Exec("CREATE TABLE " + quote(otherSchema) + ".private_data (value text)")
	require.NoError(err)

	// This role can use an archive, but cannot create or alter its tables.
	_, err = admin.Exec("CREATE ROLE " + quote(role) + " LOGIN PASSWORD 'archive_test'")
	require.NoError(err)
	for _, grant := range []string{
		"GRANT USAGE ON SCHEMA " + quote(schema) + " TO " + quote(role),
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA " + quote(schema) + " TO " + quote(role),
		"GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA " + quote(schema) + " TO " + quote(role),
	} {
		_, err = admin.Exec(grant)
		require.NoError(err)
	}
	u.User = url.UserPassword(role, "archive_test")
	opts.URL = u.String()
	runtime, err := archive.Open(t.Context(), opts)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(runtime.Close()) })
	require.Error(archive.Setup(t.Context(), opts), "runtime role must lack DDL privileges")
	runtimeDB, err := sql.Open("pgx", u.String())
	require.NoError(err)
	t.Cleanup(func() { _ = runtimeDB.Close() })
	_, err = runtimeDB.ExecContext(t.Context(), "SELECT * FROM "+quote(otherSchema)+".private_data")
	require.Error(err, "runtime role cannot read another schema even with a qualified name")

	allowed := false
	var operations []string
	handler := archiveAPI(t, runtime).Handler(func(w http.ResponseWriter, r *http.Request, op archive.Operation) bool {
		operations = append(operations, op.ID)
		if !allowed {
			http.Error(w, "reader disabled", http.StatusForbidden)
		}
		return allowed
	})
	request := func(path string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		return response
	}
	assert.Equal(http.StatusForbidden, request("/api/v1/search?q=observatory").Code)
	allowed = true
	response := request("/api/v1/search?q=observatory")
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	assert.Contains(response.Body.String(), "Launch")
	assert.Equal([]string{"searchMessages", "searchMessages"}, operations)
	// Operations beyond message reads remain available to library callers.
	assert.Equal(http.StatusOK, request("/api/v1/stats").Code)
	assert.Contains(operations, "getStats")
	allowed = false
	assert.Equal(http.StatusForbidden, request("/api/v1/stats").Code)
	allowed = true
	server := httptest.NewServer(http.StripPrefix("/archive", handler))
	t.Cleanup(server.Close)
	apiClient, err := client.New(server.URL + "/archive")
	require.NoError(err)
	detail, err := apiClient.GetMessage(t.Context(), &generated.GetMessageRequestOptions{
		PathParams: &generated.GetMessagePath{ID: id},
	})
	require.NoError(err)
	assert.Equal(body, detail.Body)
	thread, err := apiClient.GetConversation(t.Context(), &generated.GetConversationRequestOptions{
		PathParams: &generated.GetConversationPath{ID: conversation},
		Query:      &generated.GetConversationQuery{Anchor: id},
	})
	require.NoError(err)
	require.Len(thread.Messages, 1)
	assert.Equal(body, thread.Messages[0].Body)
	accounts, err := apiClient.ListCLIAccounts(t.Context())
	require.NoError(err)
	require.Len(accounts.Accounts, 1)
	assert.Equal(source.ID, accounts.Accounts[0].ID)
	exerciseSlackReplacement(t, runtime)
	exerciseDiscordCollection(t, runtime)

	// An incompatible setup must fail without trying to repair its marker.
	_, err = admin.Exec("UPDATE " + quote(schema) + ".archive_metadata SET value = 'unsupported' WHERE key = 'schema_version'")
	require.NoError(err)
	_, err = archive.Open(t.Context(), opts)
	require.Error(err)
}

func TestPostgreSQLKeywordConnection(t *testing.T) {
	require := require.New(t)
	dsn := os.Getenv("MSGVAULT_TEST_DB")
	if !store.IsPostgresURL(dsn) {
		t.Skip("requires a disposable PostgreSQL database")
	}
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(err)
	// Exercise the alternate libpq spelling against the same disposable server.
	quote := func(s string) string {
		return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", `\'`) + "'"
	}
	keyword := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable", quote(cfg.Host), cfg.Port, quote(cfg.User), quote(cfg.Password), quote(cfg.Database))
	schema := "archive_" + strings.ToLower(rand.Text())
	admin, err := pgx.Connect(t.Context(), dsn)
	require.NoError(err)
	defer func() { _ = admin.Close(context.WithoutCancel(t.Context())) }()
	defer func() {
		_, _ = admin.Exec(context.WithoutCancel(t.Context()), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	}()
	opts := archive.PostgreSQL{URL: keyword, Schema: schema}
	require.NoError(archive.Setup(t.Context(), opts))
	instance, err := archive.Open(t.Context(), opts)
	require.NoError(err)
	require.NoError(instance.Close())
}

func archiveAPI(t *testing.T, runtime *archive.Archive) *archive.Server {
	t.Helper()
	server := archive.NewServer(archive.ServerOptions{Store: runtime.Store(), Engine: runtime.QueryEngine()})
	t.Cleanup(func() { require.NoError(t, server.Shutdown(context.WithoutCancel(t.Context()))) })
	return server
}

func TestPostgreSQLSetupUsesProvisionedSchema(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dsn := os.Getenv("MSGVAULT_TEST_DB")
	if !store.IsPostgresURL(dsn) {
		t.Skip("requires a disposable PostgreSQL database")
	}
	admin, err := sql.Open("pgx", dsn)
	require.NoError(err)
	t.Cleanup(func() { _ = admin.Close() })
	schema := "provisioned_" + strings.ToLower(rand.Text())
	role := "owner_" + strings.ToLower(rand.Text())
	quote := func(name string) string { return pgx.Identifier{name}.Sanitize() }
	t.Cleanup(func() {
		_, err := admin.Exec("DROP SCHEMA IF EXISTS " + quote(schema) + " CASCADE")
		assert.NoError(err)
		_, err = admin.Exec("DROP ROLE IF EXISTS " + quote(role))
		assert.NoError(err)
	})
	// The role owns its schema but cannot create schemas in the database.
	_, err = admin.Exec("CREATE ROLE " + quote(role) + " LOGIN PASSWORD 'archive_test'")
	require.NoError(err)
	_, err = admin.Exec("CREATE SCHEMA " + quote(schema) + " AUTHORIZATION " + quote(role))
	require.NoError(err)
	u, err := url.Parse(dsn)
	require.NoError(err)
	u.User = url.UserPassword(role, "archive_test")
	opts := archive.PostgreSQL{URL: u.String(), Schema: schema}

	require.NoError(archive.Setup(t.Context(), opts))
	runtime, err := archive.Open(t.Context(), opts)
	require.NoError(err)
	assert.NoError(runtime.Close())
}
