package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/config"
)

func markDaemonCLISubprocessForTest(t *testing.T) {
	t.Helper()
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
}

type daemonCLIRunTestRequest struct {
	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`
	Cwd  string            `json:"cwd"`
}

type daemonCLIAddCalendarPlanTestRequest struct {
	Email            string `json:"email"`
	OAuthApp         string `json:"oauth_app"`
	OAuthAppExplicit bool   `json:"oauth_app_explicit"`
	Headless         bool   `json:"headless"`
}

type daemonCLIEmbeddingsPlanTestRequest struct {
	Operation    string `json:"operation"`
	GenerationID int64  `json:"generation_id"`
	Force        bool   `json:"force"`
}

type daemonCLIDeleteStagedPlanTestRequest struct {
	BatchID             string `json:"batch_id"`
	Permanent           bool   `json:"permanent"`
	Yes                 bool   `json:"yes"`
	DryRun              bool   `json:"dry_run"`
	List                bool   `json:"list"`
	Account             string `json:"account"`
	RemoteDeleteEnabled bool   `json:"remote_delete_enabled"`
}

type daemonCLIDeduplicatePlanTestRequest struct {
	Account                    string `json:"account"`
	Collection                 string `json:"collection"`
	Prefer                     string `json:"prefer"`
	ContentHash                bool   `json:"content_hash"`
	DeleteDupsFromSourceServer bool   `json:"delete_dups_from_source_server"`
}

func newDaemonCLIRunnerTestServer(
	t *testing.T,
	checkRequest func(daemonCLIRunTestRequest),
	events ...string,
) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	requests := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/cli/run", daemonCLIRunTestHandler(t, requests, checkRequest, events...))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, requests
}

func newDaemonCLIAddCalendarTestServer(
	t *testing.T,
	checkPlan func(daemonCLIAddCalendarPlanTestRequest),
	planResponse map[string]any,
	checkRun func(daemonCLIRunTestRequest),
	events ...string,
) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()

	if planResponse == nil {
		planResponse = map[string]any{"needs_scope_escalation": false}
	}

	planRequests := &atomic.Int32{}
	runRequests := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/cli/add-calendar/plan", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method, "plan method")
		planRequests.Add(1)

		var req daemonCLIAddCalendarPlanTestRequest
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&req), "decode plan request") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if checkPlan != nil {
			checkPlan(req)
		}

		w.Header().Set("Content-Type", "application/json")
		if !assert.NoError(t, json.NewEncoder(w).Encode(planResponse), "write plan response") {
			return
		}
	})
	mux.HandleFunc("/api/v1/cli/run", daemonCLIRunTestHandler(t, runRequests, checkRun, events...))

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, runRequests, planRequests
}

func newDaemonCLIEmbeddingsTestServer(
	t *testing.T,
	checkPlan func(daemonCLIEmbeddingsPlanTestRequest),
	planResponse map[string]any,
	checkRun func(daemonCLIRunTestRequest),
	events ...string,
) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()

	if planResponse == nil {
		planResponse = map[string]any{"needs_confirmation": false}
	}

	planRequests := &atomic.Int32{}
	runRequests := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/cli/embeddings/plan", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method, "plan method")
		planRequests.Add(1)

		var req daemonCLIEmbeddingsPlanTestRequest
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&req), "decode plan request") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if checkPlan != nil {
			checkPlan(req)
		}

		w.Header().Set("Content-Type", "application/json")
		if !assert.NoError(t, json.NewEncoder(w).Encode(planResponse), "write plan response") {
			return
		}
	})
	mux.HandleFunc("/api/v1/cli/run", daemonCLIRunTestHandler(t, runRequests, checkRun, events...))

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, runRequests, planRequests
}

func newDaemonCLIDeleteStagedTestServer(
	t *testing.T,
	checkPlan func(daemonCLIDeleteStagedPlanTestRequest),
	planResponse map[string]any,
	checkRun func(daemonCLIRunTestRequest),
	events ...string,
) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()

	if planResponse == nil {
		planResponse = map[string]any{"needs_execution": false}
	}

	planRequests := &atomic.Int32{}
	runRequests := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/cli/delete-staged/plan", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method, "plan method")
		planRequests.Add(1)

		var req daemonCLIDeleteStagedPlanTestRequest
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&req), "decode plan request") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if checkPlan != nil {
			checkPlan(req)
		}

		w.Header().Set("Content-Type", "application/json")
		if !assert.NoError(t, json.NewEncoder(w).Encode(planResponse), "write plan response") {
			return
		}
	})
	mux.HandleFunc("/api/v1/cli/run", daemonCLIRunTestHandler(t, runRequests, checkRun, events...))

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, runRequests, planRequests
}

func newDaemonCLIDeduplicateTestServer(
	t *testing.T,
	checkPlan func(daemonCLIDeduplicatePlanTestRequest),
	planResponse map[string]any,
	checkRun func(daemonCLIRunTestRequest),
	events ...string,
) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()

	if planResponse == nil {
		planResponse = map[string]any{"items": []map[string]any{}}
	}

	planRequests := &atomic.Int32{}
	runRequests := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/cli/deduplicate/plan", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method, "plan method")
		planRequests.Add(1)

		var req daemonCLIDeduplicatePlanTestRequest
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&req), "decode plan request") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if checkPlan != nil {
			checkPlan(req)
		}

		w.Header().Set("Content-Type", "application/json")
		if !assert.NoError(t, json.NewEncoder(w).Encode(planResponse), "write plan response") {
			return
		}
	})
	mux.HandleFunc("/api/v1/cli/run", daemonCLIRunTestHandler(t, runRequests, checkRun, events...))

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, runRequests, planRequests
}

func daemonCLIRunTestHandler(
	t *testing.T,
	requests *atomic.Int32,
	checkRequest func(daemonCLIRunTestRequest),
	events ...string,
) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method, "method")
		requests.Add(1)

		var req daemonCLIRunTestRequest
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&req), "decode request") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if checkRequest != nil {
			checkRequest(req)
		}

		w.Header().Set("Content-Type", "application/x-ndjson")
		if len(events) == 0 {
			_, _ = w.Write([]byte(`{"type":"complete"}` + "\n"))
			return
		}
		for _, event := range events {
			_, _ = w.Write([]byte(event + "\n"))
		}
	}
}

func configureRemoteDaemonForTest(t *testing.T, url string) {
	t.Helper()

	savedCfg := cfg
	savedUseLocal := useLocal
	t.Cleanup(func() {
		cfg = savedCfg
		useLocal = savedUseLocal
	})
	cfg = &config.Config{
		HomeDir: t.TempDir(),
		Remote: config.RemoteConfig{
			URL:           url,
			AllowInsecure: true,
		},
	}
	useLocal = false
}

func TestDaemonCLIArgsFromCobraForwardsCommandFlagsAndPositionals(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var (
		yes    bool
		kind   string
		labels []string
	)
	cmd := &cobra.Command{
		Use: "import-mbox <identifier> <export-file>",
		RunE: func(cmd *cobra.Command, args []string) error {
			got, err := daemonCLIArgsFromCobra(cmd, args)
			require.NoError(err, "daemon args")
			assert.Equal([]string{
				"import-mbox",
				"--label=inbox",
				"--label=archive",
				"--source-type=hey",
				"--yes",
				"alice@example.com",
				"/tmp/export.mbox",
			}, got, "daemon args")
			return nil
		},
	}
	cmd.Flags().StringVar(&kind, "source-type", "mbox", "")
	cmd.Flags().StringSliceVar(&labels, "label", nil, "")
	cmd.Flags().BoolVar(&yes, "yes", false, "")
	cmd.SetArgs([]string{
		"--label", "inbox,archive",
		"--source-type", "hey",
		"--yes",
		"alice@example.com",
		"/tmp/export.mbox",
	})

	require.NoError(cmd.Execute(), "execute")
}

func TestRunDaemonCLICommandHTTPOmitsCallerCwdForConfiguredRemote(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
		assert.Equal([]string{"import-mbox", "alice@example.com", "export.mbox"}, req.Args, "args")
		assert.Empty(req.Cwd, "configured remote must not receive caller-local cwd")
	}, `{"type":"complete"}`)
	configureRemoteDaemonForTest(t, server.URL)

	cmd := &cobra.Command{Use: "import-mbox"}
	cmd.SetContext(context.Background())
	require.NoError(runDaemonCLICommandHTTPFromCobra(cmd, []string{"alice@example.com", "export.mbox"}), "run daemon command")
	assert.Equal(1, int(requests.Load()), "runner endpoint calls")
}

func TestDaemonCLIRunCwdUsesCallerCwdForLocalDaemon(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	cwd, err := os.Getwd()
	require.NoError(err, "get cwd")

	got, err := daemonCLIRunCwd(HTTPStoreInfo{Kind: HTTPStoreLocalDaemon})

	require.NoError(err, "daemonCLIRunCwd")
	assert.Equal(cwd, got, "local daemon cwd")
}

func TestDaemonCLIArgsFromCobraHandlesNestedCommandsAndFalseBool(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := &cobra.Command{Use: "msgvault"}
	parent := &cobra.Command{Use: "import"}
	var includeSMS = true
	child := &cobra.Command{
		Use: "synctech <path>",
		RunE: func(cmd *cobra.Command, args []string) error {
			got, err := daemonCLIArgsFromCobra(cmd, args)
			require.NoError(err, "daemon args")
			assert.Equal([]string{
				"import",
				"synctech",
				"--sms=false",
				"/tmp/sms.csv",
			}, got, "daemon args")
			return nil
		},
	}
	child.Flags().BoolVar(&includeSMS, "sms", true, "")
	parent.AddCommand(child)
	root.AddCommand(parent)
	root.SetArgs([]string{"import", "synctech", "--sms=false", "/tmp/sms.csv"})

	require.NoError(root.Execute(), "execute")
}

func TestDaemonCLIArgsFromCobraSkipsRootPersistentFlags(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var cfgFile string
	var verbose bool
	root := &cobra.Command{Use: "msgvault"}
	root.PersistentFlags().StringVar(&cfgFile, "config", "", "")
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "")

	child := &cobra.Command{
		Use: "deduplicate",
		RunE: func(cmd *cobra.Command, args []string) error {
			got, err := daemonCLIArgsFromCobra(cmd, args)
			require.NoError(err, "daemon args")
			assert.Equal([]string{"deduplicate", "--dry-run"}, got, "daemon args")
			return nil
		},
	}
	var dryRun bool
	child.Flags().BoolVar(&dryRun, "dry-run", false, "")
	root.AddCommand(child)
	root.SetArgs([]string{"--config", "/tmp/config.toml", "--verbose", "deduplicate", "--dry-run"})

	require.NoError(root.Execute(), "execute")
}
