package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/credentialprovider"
)

// hostedRemote stands in for a service that hosts a msgvault archive under a
// mount path and admits only bearers its credential helper issued, for the
// read operations in credentialprovider.ReadOperations.
type hostedRemote struct {
	*httptest.Server

	mu         sync.Mutex
	operations []string
	apiKeys    []string
}

const hostedMount = "/archive"

func newHostedRemote(t *testing.T, backendURL, ownerKey, helperDir string) *hostedRemote {
	t.Helper()
	backend, err := url.Parse(backendURL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(backend)
	proxy.Transport = &http.Transport{}
	remote := &hostedRemote{}
	remote.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote.mu.Lock()
		if key := r.Header.Get("X-Api-Key"); key != "" {
			remote.apiKeys = append(remote.apiKeys, key)
		}
		remote.mu.Unlock()
		bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		issued, _ := os.ReadFile(filepath.Join(helperDir, "tokens"))
		if !ok || !slices.Contains(strings.Fields(string(issued)), bearer) {
			http.Error(w, "unknown bearer", http.StatusUnauthorized)
			return
		}
		path, ok := strings.CutPrefix(r.URL.Path, hostedMount)
		if !ok {
			http.NotFound(w, r)
			return
		}
		operation := openAPIOperationID(r.Method, path)
		remote.mu.Lock()
		remote.operations = append(remote.operations, operation)
		remote.mu.Unlock()
		if !slices.Contains(credentialprovider.ReadOperations(), operation) {
			http.Error(w, "operation not admitted", http.StatusForbidden)
			return
		}
		r.URL.Path = path
		r.Header.Del("Authorization")
		r.Header.Set("X-Api-Key", ownerKey)
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(remote.Close)
	// The CLI's HTTP client uses the default transport, which must trust the
	// hosted remote's test certificate.
	previous := http.DefaultTransport
	http.DefaultTransport = remote.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	return remote
}

func (r *hostedRemote) seen() (operations, apiKeys []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.operations), slices.Clone(r.apiKeys)
}

// openAPIOperationID names the msgvault operation a request path reaches.
func openAPIOperationID(method, path string) string {
	segments := strings.Split(path, "/")
	for template, item := range api.OpenAPIDocument().Paths {
		parts := strings.Split(template, "/")
		if len(parts) != len(segments) {
			continue
		}
		match := true
		for i, part := range parts {
			if !strings.HasPrefix(part, "{") && part != segments[i] {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		operations := map[string]string{}
		if item.Get != nil {
			operations[http.MethodGet] = item.Get.OperationID
		}
		if item.Post != nil {
			operations[http.MethodPost] = item.Post.OperationID
		}
		if item.Put != nil {
			operations[http.MethodPut] = item.Put.OperationID
		}
		if item.Patch != nil {
			operations[http.MethodPatch] = item.Patch.OperationID
		}
		if item.Delete != nil {
			operations[http.MethodDelete] = item.Delete.OperationID
		}
		if id, ok := operations[method]; ok {
			return id
		}
	}
	return method + " " + path
}

func writeHostedRemoteConfig(t *testing.T, home, remoteURL string, helperDir string, sourceID int64, extra string) {
	t.Helper()
	executable, err := os.Executable()
	require.NoError(t, err)
	argv, err := json.Marshal([]string{executable, "-test.run=^TestRemoteCredentialHelperProcess$", "--", helperDir})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`[remotes.hosted]
url = `+strconv.Quote(remoteURL+hostedMount)+`
credential_provider = `+string(argv)+`
`+extra+`
[remotes.hosted.delegation]
archive_id = "archive-a"
client_id = "22222222-2222-4222-8222-222222222222"
delegation_id = "33333333-3333-4333-8333-333333333333"
source_ids = [`+strconv.FormatInt(sourceID, 10)+`]
`), 0o600))
}

func readHelperRequests(t *testing.T, dir string) []credentialprovider.Request {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "requests.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var requests []credentialprovider.Request
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		request, err := credentialprovider.DecodeRequest(bytes.NewReader(scanner.Bytes()))
		require.NoError(t, err)
		requests = append(requests, request)
	}
	return requests
}

func runHostedCommand(t *testing.T, home string, args ...string) (string, error) {
	t.Helper()
	// Other tests leave flags set on the shared commands; start from defaults.
	command, _, err := rootCmd.Find(args)
	require.NoError(t, err)
	command.Flags().VisitAll(func(flag *pflag.Flag) {
		if slice, ok := flag.Value.(pflag.SliceValue); ok {
			require.NoError(t, slice.Replace(nil))
		} else {
			require.NoError(t, flag.Value.Set(flag.DefValue))
		}
		flag.Changed = false
	})
	done := captureStdout(t)
	output, err := runRootForTest(t, home, append([]string{"--remote", "hosted"}, args...)...)
	return output + done(), err
}

func TestCredentialHelperRemoteRunsReadCommands(t *testing.T) { //nolint:paralleltest // replaces the default HTTP transport
	require := require.New(t)
	assert := assert.New(t)
	t.Cleanup(func() { clearInvocationFlags(rootCmd) })
	previousCheck := remoteAPISchemaCheckEnabled
	remoteAPISchemaCheckEnabled = true
	t.Cleanup(func() { remoteAPISchemaCheckEnabled = previousCheck })

	st := testutil.NewTestStore(t)
	source, id, err := testutil.CreateIndexedSourceMessage(t.Context(), st, "reader@example.test", "message-1", "glacier", "synthetic body")
	require.NoError(err)
	cfg := &config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key"}}
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st}, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { require.NoError(server.Shutdown(context.WithoutCancel(t.Context()))) })
	backend := httptest.NewServer(server.Router())
	t.Cleanup(backend.Close)
	helperDir := t.TempDir()
	remote := newHostedRemote(t, backend.URL, "synthetic-owner-key", helperDir)
	home := t.TempDir()
	writeHostedRemoteConfig(t, home, remote.URL, helperDir, source.ID, "")
	messageID := strconv.FormatInt(id, 10)

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"list-accounts"}, "reader@example.test"},
		{[]string{"stats"}, "Messages:"},
		{[]string{"search", "glacier", "--json"}, "glacier"},
		{[]string{"show-message", messageID, "--json"}, "synthetic body"},
		{[]string{"show-thread", messageID, "--json"}, "synthetic body"},
		{[]string{"export-messages", "--start", "1970-01-02T00:00:00Z", "--end", "2200-01-01T00:00:00Z"}, `"record_type":"complete"`},
	} {
		output, err := runHostedCommand(t, home, tc.args...)
		require.NoError(err, "%s", tc.args[0])
		assert.Contains(output, tc.want, "%s", tc.args[0])
	}

	for _, mode := range []string{"vector", "hybrid"} {
		_, err := runHostedCommand(t, home, "search", "glacier", "--mode", mode)
		require.ErrorContains(err, "use --mode=fts", "semantic search is outside the helper's operations")
	}

	operations, apiKeys := remote.seen()
	assert.Empty(apiKeys, "a helper-backed remote never sends an API key")
	slices.Sort(operations)
	assert.Equal([]string{"exportCLIMessages", "getCLIMessage", "getCLIMessageThread", "getCLIStats", "getHealth", "listCLIAccounts", "searchCLI"}, slices.Compact(operations),
		"the read commands call only admitted operations")

	requests := readHelperRequests(t, helperDir)
	require.Len(requests, 6, "each read command asks the helper once; refused searches do not")
	assert.Equal("authorize", requests[0].Operation)
	for i, request := range requests {
		assert.Equal(remote.URL+hostedMount, request.Endpoint)
		assert.Equal([]string{strconv.FormatInt(source.ID, 10)}, request.SourceIDs)
		assert.Equal(credentialprovider.ReadOperations(), request.Operations)
		if i > 0 {
			assert.Equal("replace", request.Operation, "a later command replaces the bearer an earlier one held")
			assert.Equal(requests[i-1].RequestID, request.PreviousRequestID)
		}
	}
}

func TestCredentialHelperRemoteNeverFallsBackToAnAPIKey(t *testing.T) { //nolint:paralleltest // replaces the default HTTP transport and environment
	require := require.New(t)
	assert := assert.New(t)
	t.Cleanup(func() { clearInvocationFlags(rootCmd) })
	backend := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(backend.Close)
	helperDir := t.TempDir()
	remote := newHostedRemote(t, backend.URL, "synthetic-owner-key", helperDir)
	home := t.TempDir()
	t.Setenv("MSGVAULT_REMOTE_API_KEY", "synthetic-environment-key")
	require.NoError(os.WriteFile(filepath.Join(helperDir, "mode"), []byte("sign-in"), 0o600))
	writeHostedRemoteConfig(t, home, remote.URL, helperDir, 17, "")

	_, err := runHostedCommand(t, home, "list-accounts")
	require.ErrorContains(err, "sign_in_required")
	operations, apiKeys := remote.seen()
	assert.Empty(operations, "a refused helper sends no request")
	assert.Empty(apiKeys)

	writeHostedRemoteConfig(t, home, remote.URL, helperDir, 17, `api_key = "synthetic-config-key"`)
	_, err = runHostedCommand(t, home, "list-accounts")
	require.ErrorContains(err, "cannot also set api_key")
	assert.Len(readHelperRequests(t, helperDir), 1, "a conflicting config never starts the helper")

	writeHostedRemoteConfig(t, home, remote.URL, helperDir, 17, "")
	_, err = runHostedCommand(t, home, "export-token", "reader@example.test")
	require.ErrorContains(err, "pass --to and --api-key", "export-token never picks up the environment's key")
	operations, apiKeys = remote.seen()
	assert.Empty(operations)
	assert.Empty(apiKeys)
}

// TestRemoteCredentialHelperProcess is the hosted remote's credential helper.
// It issues a 15-minute bearer for each request and records both.
func TestRemoteCredentialHelperProcess(t *testing.T) {
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) != 2 {
		return
	}
	dir := args[1]
	request, err := credentialprovider.DecodeRequest(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	appendLine := func(name string, line []byte) {
		file, err := os.OpenFile(filepath.Join(dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(1)
		}
		if _, err := file.Write(append(line, '\n')); err != nil || file.Close() != nil {
			os.Exit(1)
		}
	}
	line, err := json.Marshal(request)
	if err != nil {
		os.Exit(1)
	}
	appendLine("requests.jsonl", line)
	response := credentialprovider.Response{Version: request.Version, Operation: request.Operation, RequestID: request.RequestID}
	if mode, _ := os.ReadFile(filepath.Join(dir, "mode")); string(mode) == "sign-in" {
		response.Status, response.Reason = credentialprovider.StatusSignInRequired, "no_current_sign_in"
	} else {
		generation := request.ExpectedGeneration + 1
		token := "synthetic-hosted-bearer-" + strconv.FormatInt(generation, 10)
		appendLine("tokens", []byte(token))
		now := time.Now().UTC()
		response.Scope = request.Scope
		response.Status = credentialprovider.StatusReady
		response.CredentialID = uuid.New()
		response.CredentialGeneration = generation
		response.IssuedAt, response.ExpiresAt = now, now.Add(15*time.Minute)
		response.Credential = &credentialprovider.Credential{Type: "bearer", Token: token}
	}
	if credentialprovider.WriteResponse(os.Stdout, request, response) != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
