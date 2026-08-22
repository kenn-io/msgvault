package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestCardDAVCommandsExposeSafeOperatorSurface(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	add := newAddCardDAVCmd()
	assert.Nil(add.Flags().Lookup("password"), "passwords must never be accepted on argv")
	assert.Equal("add-carddav", add.Name())

	root := newCardDAVCmd()
	books, _, err := root.Find([]string{"books"})
	require.NoError(err)
	assert.Equal("books", books.Name())
	setRole, _, err := root.Find([]string{"books", "set-role"})
	require.NoError(err)
	assert.Equal("set-role", setRole.Name())
	resolve, _, err := root.Find([]string{"conflicts", "resolve"})
	require.NoError(err)
	assert.Equal("resolve", resolve.Name())
	show, _, err := root.Find([]string{"conflicts", "show"})
	require.NoError(err)
	assert.Equal("show", show.Name())
}

func TestCardDAVCLIProductionRoutes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	requests := make([]string, 0, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "PUT /api/v1/carddav/account":
			var body map[string]any
			assert.NoError(json.NewDecoder(r.Body).Decode(&body))
			assert.Equal("https://contacts.example/dav", body["base_url"])
			assert.Equal("alice", body["username"])
			assert.Equal("synthetic-password", body["password"])
			assert.Equal("0 3 * * *", body["schedule"])
			_, _ = w.Write([]byte(`{"base_url":"https://contacts.example/dav","username":"alice","enabled":true,"schedule":"0 3 * * *","books":1}`))
		case "GET /api/v1/carddav/books":
			_, _ = w.Write([]byte(`{"books":[{"id":9,"name":"Personal","url":"https://contacts.example/books/personal/","write_target":true,"subscribed":true,"lookup_source":false,"needs_full_reconcile":false}]}`))
		case "PATCH /api/v1/carddav/books/9":
			var body map[string]bool
			assert.NoError(json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(map[string]bool{"write_target": true, "subscribed": true, "lookup_source": true}, body)
			_, _ = w.Write([]byte(`{"id":9,"name":"Personal","url":"https://contacts.example/books/personal/","write_target":true,"subscribed":true,"lookup_source":true,"needs_full_reconcile":false}`))
		case "GET /api/v1/carddav/conflicts":
			_, _ = w.Write([]byte(`{"conflicts":[]}`))
		case "GET /api/v1/carddav/conflicts/7":
			_, _ = w.Write([]byte(`{"id":7,"address_book_id":9,"href":"/alice.vcf","local_tombstone":false,"local_vcard":"BEGIN:VCARD\\r\\nFN:Local Alice\\r\\nEND:VCARD\\r\\n","remote_tombstone":false,"remote_vcard":"BEGIN:VCARD\\r\\nFN:Remote Alice\\r\\nEND:VCARD\\r\\n","status":"open"}`))
		case "POST /api/v1/carddav/conflicts/7/resolve":
			var body map[string]string
			assert.NoError(json.NewDecoder(r.Body).Decode(&body))
			assert.Equal("keep_remote", body["choice"])
			_, _ = w.Write([]byte(`{"id":7,"address_book_id":9,"href":"/alice.vcf","local_tombstone":false,"remote_tombstone":false,"status":"resolved"}`))
		case "POST /api/v1/carddav/publications/11":
			_, _ = w.Write([]byte(`{"person_id":11,"desired":true}`))
		case "DELETE /api/v1/carddav/publications/11":
			_, _ = w.Write([]byte(`{"person_id":11,"desired":false}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	home := t.TempDir()
	withStoreResolverConfig(t, &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}, Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})

	readEnd, writeEnd, err := os.Pipe()
	require.NoError(err)
	_, err = writeEnd.WriteString("synthetic-password\n")
	require.NoError(err)
	require.NoError(writeEnd.Close())
	originalStdin := os.Stdin
	os.Stdin = readEnd
	t.Cleanup(func() { os.Stdin = originalStdin; _ = readEnd.Close() })
	add := newAddCardDAVCmd()
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
		{cmd: newPersonCardDAVCommand("unpublish", false), args: []string{"11"}},
	} {
		invocation.cmd.SetOut(&bytes.Buffer{})
		invocation.cmd.SetErr(&bytes.Buffer{})
		invocation.cmd.SetArgs(invocation.args)
		require.NoError(invocation.cmd.Execute())
	}

	assert.Equal([]string{
		"PUT /api/v1/carddav/account",
		"GET /api/v1/carddav/books",
		"PATCH /api/v1/carddav/books/9",
		"GET /api/v1/carddav/conflicts",
		"GET /api/v1/carddav/conflicts/7",
		"POST /api/v1/carddav/conflicts/7/resolve",
		"POST /api/v1/carddav/publications/11",
		"DELETE /api/v1/carddav/publications/11",
	}, requests)
	assert.NotContains(strings.Join(requests, "\n"), "synthetic-password")
}

func TestCardDAVConflictShowPrintsSnapshotsAsSafeJSON(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	localVCard := "BEGIN:VCARD\r\nFN:\x1b[31mLocal Alice\x1b[0m\r\nEND:VCARD\r\n"
	remoteVCard := "BEGIN:VCARD\r\nFN:Remote Alice\r\nEND:VCARD\r\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodGet, r.Method)
		assert.Equal("/api/v1/carddav/conflicts/7", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(json.NewEncoder(w).Encode(map[string]any{
			"id": 7, "address_book_id": 9, "href": "/alice.vcf",
			"local_tombstone": false, "local_vcard": localVCard,
			"remote_tombstone": false, "remote_vcard": remoteVCard, "status": "open",
		}))
	}))
	t.Cleanup(server.Close)
	home := t.TempDir()
	withStoreResolverConfig(t, &config.Config{
		HomeDir: home, Data: config.DataConfig{DataDir: home},
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	var stdout bytes.Buffer
	cmd := newCardDAVCmd()
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"conflicts", "show", "7"})
	require.NoError(cmd.Execute())
	assert.NotContains(stdout.String(), "\x1b")
	var got struct {
		LocalVCard  *string `json:"local_vcard"`
		RemoteVCard *string `json:"remote_vcard"`
	}
	require.NoError(json.Unmarshal(stdout.Bytes(), &got))
	require.NotNil(got.LocalVCard)
	require.NotNil(got.RemoteVCard)
	assert.Equal(localVCard, *got.LocalVCard)
	assert.Equal(remoteVCard, *got.RemoteVCard)
}

func TestCardDAVBooksSanitizesTerminalControls(t *testing.T) {
	assert := assert.New(t)
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
		assert.Equal(http.MethodGet, r.Method)
		assert.Equal("/api/v1/carddav/books", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(json.NewEncoder(w).Encode(struct {
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
	withStoreResolverConfig(t, &config.Config{
		HomeDir: home, Data: config.DataConfig{DataDir: home},
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	var stdout bytes.Buffer
	cmd := newCardDAVCmd()
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"books"})
	require.NoError(cmd.Execute())
	assert.NotContains(stdout.String(), "\x1b")
	assert.NotContains(stdout.String(), "https://attacker.test")
	assert.Contains(stdout.String(), "Personal link")
}

func TestSyncCardDAVUsesTheDaemonServiceRoute(t *testing.T) {
	var full bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/carddav/sync", r.URL.Path)
		var body struct {
			Full bool `json:"full"`
		}
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		full = body.Full
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"books":1,"created":2,"updated":3,"removed":4}`))
	}))
	t.Cleanup(server.Close)

	home := t.TempDir()
	withStoreResolverConfig(t, &config.Config{
		HomeDir: home,
		Data:    config.DataConfig{DataDir: home},
		Remote:  config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})
	var stdout bytes.Buffer
	cmd := newSyncCardDAVCmd()
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"--full"})
	require.NoError(t, cmd.Execute())
	assert.True(t, full)
	assert.Equal(t, "CardDAV sync: 1 books, 2 created, 3 updated, 4 removed\n", stdout.String())
}
