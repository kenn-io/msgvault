package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestCardDAVCommandsExposeSafeOperatorSurface(t *testing.T) {
	assertions := assert.New(t)
	require := require.New(t)

	add := newAddCardDAVCmd()
	assertions.Nil(add.Flags().Lookup("password"), "passwords must never be accepted on argv")
	assertions.Equal("add-carddav", add.Name())

	root := newCardDAVCmd()
	books, _, err := root.Find([]string{"books"})
	require.NoError(err)
	assertions.Equal("books", books.Name())
	setRole, _, err := root.Find([]string{"books", "set-role"})
	require.NoError(err)
	assertions.Equal("set-role", setRole.Name())
	resolve, _, err := root.Find([]string{"conflicts", "resolve"})
	require.NoError(err)
	assertions.Equal("resolve", resolve.Name())
	show, _, err := root.Find([]string{"conflicts", "show"})
	require.NoError(err)
	assertions.Equal("show", show.Name())
	assertions.Equal("Show safe base, local, and remote summaries for a CardDAV conflict", show.Short)
	authorize, _, err := root.Find([]string{"authorize-microsoft"})
	require.NoError(err)
	assertions.Equal("authorize-microsoft", authorize.Name())
	assertions.NotNil(authorize.Flags().Lookup("headless"))
	assertions.Nil(authorize.Flags().Lookup("schedule"), "sign-in must not change a saved connection")
}

func TestCardDAVCLIProductionRoutes(t *testing.T) {
	assertions := assert.New(t)
	require := require.New(t)

	requests := make([]string, 0, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "PUT /api/v1/carddav/account":
			var body map[string]any
			assertions.NoError(json.NewDecoder(r.Body).Decode(&body))
			assertions.Equal("https://contacts.example/dav", body["base_url"])
			assertions.Equal("alice", body["username"])
			assertions.Equal("synthetic-password", body["password"])
			assertions.Equal("0 3 * * *", body["schedule"])
			_, _ = w.Write([]byte(`{"base_url":"https://contacts.example/dav","username":"alice","enabled":true,"schedule":"0 3 * * *","books":1}`))
		case "GET /api/v1/carddav/books":
			_, _ = w.Write([]byte(`{"books":[{"id":9,"name":"Personal","url":"https://contacts.example/books/personal/","write_target":true,"subscribed":true,"lookup_source":false,"needs_full_reconcile":false}]}`))
		case "PATCH /api/v1/carddav/books/9":
			var body map[string]bool
			assertions.NoError(json.NewDecoder(r.Body).Decode(&body))
			assertions.Equal(map[string]bool{"write_target": true, "subscribed": true, "lookup_source": true}, body)
			_, _ = w.Write([]byte(`{"id":9,"name":"Personal","url":"https://contacts.example/books/personal/","write_target":true,"subscribed":true,"lookup_source":true,"needs_full_reconcile":false}`))
		case "GET /api/v1/carddav/conflicts":
			_, _ = w.Write([]byte(`{"conflicts":[]}`))
		case "GET /api/v1/carddav/conflicts/7":
			_, _ = w.Write([]byte(`{"id":7,"address_book":{"id":9,"name":"Personal"},"status":"unresolved","base":{"state":"unavailable","emails":[],"phones":[]},"local":{"state":"present","display_name":"Local Alice","emails":["local@example.test"],"phones":[]},"remote":{"state":"present","display_name":"Remote Alice","emails":[],"phones":["+12025550123"]},"allowed_resolutions":["keep_local","keep_remote"],"created_at":"2026-08-28T09:10:11Z","updated_at":"2026-08-28T10:11:12Z"}`))
		case "POST /api/v1/carddav/conflicts/7/resolve":
			var body map[string]string
			assertions.NoError(json.NewDecoder(r.Body).Decode(&body))
			assertions.Equal("keep_remote", body["choice"])
			_, _ = w.Write([]byte(`{"id":7,"status":"resolved","resolution":"keep_remote"}`))
		case "POST /api/v1/carddav/publications/11":
			_, _ = w.Write([]byte(`{"person_id":11,"state":"published","desired":true,"address_book":{"id":9,"name":"Personal"}}`))
		case "GET /api/v1/carddav/publications/11/preview":
			_, _ = w.Write([]byte(`{"person_id":11,"address_book":{"id":9,"name":"Personal"},"kind":"current","vcard":"BEGIN:VCARD\r\nEND:VCARD\r\n","approval_token":"synthetic-token","review_required":true}`))
		case "POST /api/v1/carddav/publications/11/approve":
			var body map[string]string
			assertions.NoError(json.NewDecoder(r.Body).Decode(&body))
			assertions.Equal("synthetic-token", body["approval_token"])
			_, _ = w.Write([]byte(`{"person_id":11,"state":"published","desired":true,"address_book":{"id":9,"name":"Personal"}}`))
		case "DELETE /api/v1/carddav/publications/11":
			_, _ = w.Write([]byte(`{"person_id":11,"state":"unpublished","desired":false,"address_book":{"id":9,"name":"Personal"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	home := t.TempDir()
	testCtx := withStoreResolverConfig(t, &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}, Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})

	readEnd, writeEnd, err := os.Pipe()
	require.NoError(err)
	_, err = writeEnd.WriteString("synthetic-password\n")
	require.NoError(err)
	require.NoError(writeEnd.Close())
	originalStdin := os.Stdin
	os.Stdin = readEnd
	t.Cleanup(func() { os.Stdin = originalStdin; _ = readEnd.Close() })
	add := newAddCardDAVCmd()
	add.SetContext(testCtx)
	add.SetOut(&bytes.Buffer{})
	add.SetArgs([]string{"https://contacts.example/dav", "alice", "--schedule", "0 3 * * *"})
	require.NoError(add.Execute())
	os.Stdin = originalStdin

	for _, invocation := range []struct {
		cmd  *cobra.Command
		args []string
	}{
		{cmd: newCardDAVCmd(), args: []string{"books"}},
		{cmd: newCardDAVCmd(), args: []string{"books", "set-role", "9", "--write-target", "--subscribed", "--lookup-source"}},
		{cmd: newCardDAVCmd(), args: []string{"conflicts", "list"}},
		{cmd: newCardDAVCmd(), args: []string{"conflicts", "show", "7"}},
		{cmd: newCardDAVCmd(), args: []string{"conflicts", "resolve", "7", "keep_remote"}},
		{cmd: newPersonCardDAVCommand("publish", true), args: []string{"11"}},
		{cmd: newPersonCardDAVCommand("publish", true), args: []string{"11", "--preview"}},
		{cmd: newPersonCardDAVCommand("publish", true), args: []string{"11", "--approve", "synthetic-token"}},
		{cmd: newPersonCardDAVCommand("unpublish", false), args: []string{"11"}},
	} {
		out := &bytes.Buffer{}
		invocation.cmd.SetContext(testCtx)
		invocation.cmd.SetOut(out)
		invocation.cmd.SetErr(&bytes.Buffer{})
		invocation.cmd.SetArgs(invocation.args)
		require.NoError(invocation.cmd.Execute())
		if slices.Contains(invocation.args, "--preview") {
			assertions.Contains(out.String(), `"approval_token":"synthetic-token"`)
			assertions.Contains(out.String(), `"vcard":"BEGIN:VCARD`)
		}
	}

	assertions.Equal([]string{
		"PUT /api/v1/carddav/account",
		"GET /api/v1/carddav/books",
		"PATCH /api/v1/carddav/books/9",
		"GET /api/v1/carddav/conflicts",
		"GET /api/v1/carddav/conflicts/7",
		"POST /api/v1/carddav/conflicts/7/resolve",
		"POST /api/v1/carddav/publications/11",
		"GET /api/v1/carddav/publications/11/preview",
		"POST /api/v1/carddav/publications/11/approve",
		"DELETE /api/v1/carddav/publications/11",
	}, requests)
	assertions.NotContains(strings.Join(requests, "\n"), "synthetic-password")
}

func TestCardDAVConflictShowPrintsSafeSummariesWithoutRawVCardFields(t *testing.T) {
	assertions := assert.New(t)
	require := require.New(t)
	const localRawMarker = "synthetic-local-raw-card"
	const remoteRawMarker = "synthetic-remote-raw-card"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal(http.MethodGet, r.Method)
		assertions.Equal("/api/v1/carddav/conflicts/7", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		assertions.NoError(json.NewEncoder(w).Encode(map[string]any{
			"id": 7, "address_book": map[string]any{"id": 9, "name": "Personal"},
			"status": "resolved", "resolution": "keep_remote",
			"base": map[string]any{"state": "unavailable", "emails": []string{}, "phones": []string{}},
			"local": map[string]any{
				"state": "present", "display_name": "Local Alice",
				"emails": []string{"local@example.test"}, "phones": []string{"+12025550123"},
			},
			"remote":              map[string]any{"state": "deleted", "emails": []string{}, "phones": []string{}},
			"allowed_resolutions": []string{},
			"created_at":          "2026-08-28T09:10:11Z",
			"updated_at":          "2026-08-28T10:11:12Z",
			"resolved_at":         "2026-08-28T11:12:13Z",
			"local_vcard":         localRawMarker,
			"remote_vcard":        remoteRawMarker,
		}))
	}))
	t.Cleanup(server.Close)
	home := t.TempDir()
	testCtx := withStoreResolverConfig(t, &config.Config{
		HomeDir: home, Data: config.DataConfig{DataDir: home},
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	var stdout bytes.Buffer
	cmd := newCardDAVCmd()
	cmd.SetContext(testCtx)
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"conflicts", "show", "7"})
	require.NoError(cmd.Execute())
	assertions.JSONEq(`{
		"id":7,
		"address_book":{"id":9,"name":"Personal"},
		"status":"resolved",
		"resolution":"keep_remote",
		"base":{"state":"unavailable","emails":[],"phones":[]},
		"local":{"state":"present","display_name":"Local Alice","emails":["local@example.test"],"phones":["+12025550123"]},
		"remote":{"state":"deleted","emails":[],"phones":[]},
		"allowed_resolutions":[],
		"created_at":"2026-08-28T09:10:11Z",
		"updated_at":"2026-08-28T10:11:12Z",
		"resolved_at":"2026-08-28T11:12:13Z"
	}`, stdout.String())
	assertions.NotContains(stdout.String(), "local_vcard")
	assertions.NotContains(stdout.String(), "remote_vcard")
	assertions.NotContains(stdout.String(), localRawMarker)
	assertions.NotContains(stdout.String(), remoteRawMarker)
}

func TestCardDAVBooksSanitizesTerminalControls(t *testing.T) {
	assertions := assert.New(t)
	require := require.New(t)
	type bookResponse struct {
		ID                 int64  `json:"id"`
		Name               string `json:"name"`
		URL                string `json:"url"`
		WriteTarget        bool   `json:"write_target"`
		Subscribed         bool   `json:"subscribed"`
		LookupSource       bool   `json:"lookup_source"`
		NeedsFullReconcile bool   `json:"needs_full_reconcile"`
	}
	malicious := "\x1b[31mPersonal\x1b[0m \x1b]8;;https://attacker.test\x07link\x1b]8;;\x07"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal(http.MethodGet, r.Method)
		assertions.Equal("/api/v1/carddav/books", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		assertions.NoError(json.NewEncoder(w).Encode(struct {
			Books []bookResponse `json:"books"`
		}{
			Books: []bookResponse{{
				ID: 9, Name: malicious, URL: "https://contacts.example/books/personal/",
				WriteTarget: true, Subscribed: true,
			}},
		}))
	}))
	t.Cleanup(server.Close)
	home := t.TempDir()
	testCtx := withStoreResolverConfig(t, &config.Config{
		HomeDir: home, Data: config.DataConfig{DataDir: home},
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	var stdout bytes.Buffer
	cmd := newCardDAVCmd()
	cmd.SetContext(testCtx)
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"books"})
	require.NoError(cmd.Execute())
	assertions.NotContains(stdout.String(), "\x1b")
	assertions.NotContains(stdout.String(), "https://attacker.test")
	assertions.Contains(stdout.String(), "Personal link")
}

func TestSyncCardDAVUsesTheDaemonServiceRoute(t *testing.T) {
	assertions := assert.New(t)

	var full bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal(http.MethodPost, r.Method)
		assertions.Equal("/api/v1/carddav/sync", r.URL.Path)
		var body struct {
			Full bool `json:"full"`
		}
		assertions.NoError(json.NewDecoder(r.Body).Decode(&body))
		full = body.Full
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"books":1,"created":2,"updated":3,"removed":4}`))
	}))
	t.Cleanup(server.Close)

	home := t.TempDir()
	testCtx := withStoreResolverConfig(t, &config.Config{
		HomeDir: home,
		Data:    config.DataConfig{DataDir: home},
		Remote:  config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})
	var stdout bytes.Buffer
	cmd := newSyncCardDAVCmd()
	cmd.SetContext(testCtx)
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"--full"})
	require.NoError(t, cmd.Execute())
	assertions.True(full)
	assertions.Equal("CardDAV sync: 1 books, 2 created, 3 updated, 4 removed\n", stdout.String())
}

func TestCardDAVCLISelectsConnectionsAndReportsAggregateFailures(t *testing.T) {
	for _, state := range []string{"succeeded", "partial", "failed"} {
		t.Run(state, func(t *testing.T) {
			assertions := assert.New(t)
			require := require.New(t)

			var requests []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r.URL.RequestURI())
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/carddav/connections":
					_, _ = w.Write([]byte(`{"connections":[{"connection":"work","account_id":2,"orphaned":false,"status":{"enabled":true,"available":true}},{"connection":"old","account_id":3,"orphaned":true,"status":{"enabled":false,"available":false}}]}`))
				case "/api/v1/carddav/books":
					assertions.Equal("work", r.URL.Query().Get("connection"))
					_, _ = w.Write([]byte(`{"books":[{"id":9,"account_id":2,"connection":"work","name":"Work contacts","url":"https://contacts.example/books/","write_target":false,"subscribed":true,"lookup_source":false,"needs_full_reconcile":false}]}`))
				case "/api/v1/carddav/sync":
					var body map[string]any
					if !assertions.NoError(json.NewDecoder(r.Body).Decode(&body)) {
						http.Error(w, "invalid synthetic request", http.StatusBadRequest)
						return
					}
					if body["connection"] != nil {
						assertions.Equal("work", body["connection"])
						_, _ = w.Write([]byte(`{"books":1,"created":0,"updated":0,"removed":0}`))
						return
					}
					_, _ = w.Write([]byte(`{"books":1,"created":0,"updated":0,"removed":0,"status":"` + state + `","connections":[{"connection":"work","status":"` + state + `","error_code":"connection_unavailable","error_message":"Connection unavailable","books":0,"created":0,"updated":0,"removed":0}]}`))
				case "/api/v1/carddav/account":
					var body map[string]any
					if !assertions.NoError(json.NewDecoder(r.Body).Decode(&body)) {
						http.Error(w, "invalid synthetic request", http.StatusBadRequest)
						return
					}
					assertions.Equal("work", body["connection"])
					assertions.Equal("google", body["provider"])
					_, _ = w.Write([]byte(`{"base_url":"https://www.googleapis.com/carddav/v1/principals/person@example.com/lists/","username":"person@example.com","enabled":true,"books":1}`))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			home := t.TempDir()
			ctx := withStoreResolverConfig(t, &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}, Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
			for _, invocation := range []struct {
				cmd     *cobra.Command
				args    []string
				failure bool
			}{
				{newCardDAVCmd(), []string{"connections"}, false},
				{newCardDAVCmd(), []string{"books", "--connection", "work"}, false},
				{newAddCardDAVCmd(), []string{"--google", "person@example.com", "--connection", "work"}, false},
				{newSyncCardDAVCmd(), []string{"--connection", "work"}, false},
				{newSyncCardDAVCmd(), nil, state != "succeeded"},
			} {
				var out, stderr bytes.Buffer
				invocation.cmd.SetContext(ctx)
				invocation.cmd.SetOut(&out)
				invocation.cmd.SetErr(&stderr)
				invocation.cmd.SetArgs(invocation.args)
				err := invocation.cmd.Execute()
				if slices.Equal(invocation.args, []string{"connections"}) {
					assertions.Contains(out.String(), "orphaned (restore configuration)")
				}
				if invocation.failure {
					require.Error(err)
					assertions.Contains(stderr.String(), "work")
					assertions.Contains(stderr.String(), "connection_unavailable")
				} else {
					require.NoError(err)
				}
				if len(invocation.args) > 0 && (invocation.args[0] == "books" || invocation.args[0] == "connections") {
					assertions.Contains(out.String(), "work")
				}
			}
			assertions.Len(requests, 5)
		})
	}
}

func TestCardDAVCLIRejectsInvalidConnectionBeforeRequest(t *testing.T) {
	for _, command := range []*cobra.Command{newAddCardDAVCmd(), newSyncCardDAVCmd(), newCardDAVCmd()} {
		args := []string{"--connection", "../invalid"}
		switch command.Name() {
		case "add-carddav":
			args = append(args, "https://contacts.example/dav", "person")
		case "carddav":
			args = append([]string{"books"}, args...)
		}
		command.SetOut(&bytes.Buffer{})
		command.SetErr(&bytes.Buffer{})
		command.SetArgs(args)
		err := command.Execute()
		require.Error(t, err)
		assert.ErrorContains(t, err, "connection")
	}
}

func TestAddCardDAVMicrosoftFlagsAreCheckedBeforeSignIn(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--google", "--microsoft", "person@example.com"}, "--google and --microsoft cannot be combined"},
		{[]string{"--headless", "https://contacts.example/dav", "person"}, "--headless requires --microsoft"},
		{[]string{"--microsoft", "--oauth-app", "contacts", "person@example.com"}, "--oauth-app requires --google"},
	} {
		command := newAddCardDAVCmd()
		command.SetOut(&bytes.Buffer{})
		command.SetErr(&bytes.Buffer{})
		command.SetArgs(tc.args)
		require.ErrorContains(t, command.Execute(), tc.want)
	}
}
