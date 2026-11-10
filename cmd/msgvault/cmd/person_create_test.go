package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPersonCreateCLIReportsPublishFailureWithRetry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var creates, publishes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/people/create":
			creates++
			var input map[string]any
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&input)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			assert.Equal("Alex Example", input["name"])
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":7,"display_name":"Alex Example","vcard_uid":"new-person","revision":1,"participant_ids":[],"created_at":"2026-10-08T12:00:00Z","updated_at":"2026-10-08T12:00:00Z"}`))
		case "/api/v1/carddav/publications/7":
			publishes++
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"carddav_inference_review_required","message":"Preview and approve publication"}`))
		default:
			assert.Fail("unexpected path", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	home := t.TempDir()
	testCtx := withStoreResolverConfig(t, &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}, Remote: config.RemoteConfig{URL: server.URL, APIKey: "test-key", AllowInsecure: true}})
	for _, publish := range []bool{false, true} {
		command := newPersonCreateCommand()
		command.SetContext(testCtx)
		var out bytes.Buffer
		command.SetOut(&out)
		args := []string{"--name", "Alex Example", "--email", "alex@example.com:work", "--phone", "+12025550123:cell", "--json"}
		if publish {
			args = append(args, "--publish")
		}
		command.SetArgs(args)
		err := command.Execute()
		if publish {
			require.Error(err)
			assert.Contains(err.Error(), "person 7 was created")
			assert.Contains(err.Error(), "person publish 7")
		} else {
			require.NoError(err)
		}
		assert.Contains(out.String(), `"id":7`)
	}
	assert.Equal(2, creates)
	assert.Equal(1, publishes)
}

func TestPersonCreateCLIProductionAdapter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	serverCfg := &config.Config{Server: config.ServerConfig{APIKey: "test-key"}}
	srv := api.NewServerWithOptions(api.ServerOptions{Config: serverCfg, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)})
	httpServer := httptest.NewServer(srv.Router())
	t.Cleanup(httpServer.Close)
	home := t.TempDir()
	testCtx := withStoreResolverConfig(t, &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}, Remote: config.RemoteConfig{URL: httpServer.URL, APIKey: "test-key", AllowInsecure: true}})
	command := newPersonCreateCommand()
	command.SetContext(testCtx)
	command.SetOut(io.Discard)
	command.SetArgs([]string{"--name", "Alex Example", "--email", "alex@example.com:work", "--note", "Met at a conference"})
	require.NoError(command.Execute())
	people, err := st.ListPersonsContext(t.Context())
	require.NoError(err)
	require.Len(people, 1)
	assert.Empty(people[0].ParticipantIDs)
	points, err := st.ListPersonContactPointsContext(t.Context(), people[0].ID, true)
	require.NoError(err)
	require.Len(points, 1)
	assert.Equal("alex@example.com", points[0].NormalizedValue)
	duplicate := newPersonCreateCommand()
	duplicate.SetContext(testCtx)
	duplicate.SetOut(io.Discard)
	duplicate.SetArgs([]string{"--name", "Duplicate", "--email", "ALEX@example.com"})
	require.ErrorContains(duplicate.Execute(), "matches person")
}

func TestParsePersonCreateContacts(t *testing.T) {
	for _, tc := range []struct {
		raw, value, kind string
	}{
		{"alex@example.com", "alex@example.com", ""},
		{"alex@example.com:work", "alex@example.com", "work"},
		{"+12025550123", "+12025550123", ""},
		{"+12025550123:cell", "+12025550123", "cell"},
		{"+1 (202) 555-0123:x-assistant", "+1 (202) 555-0123", "x-assistant"},
	} {
		got := parsePersonCreateContacts([]string{tc.raw})
		require.Len(t, got, 1, tc.raw)
		assert.Equal(t, tc.value, got[0].Value, tc.raw)
		assert.Equal(t, tc.kind, got[0].Type, tc.raw)
	}
}

func TestPersonCreateCLIRejectsTitleWithoutOrg(t *testing.T) {
	command := newPersonCreateCommand()
	command.SetContext(t.Context())
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"--name", "Alex Example", "--title", "Engineer"})
	require.ErrorContains(t, command.Execute(), "title requires org")
}

func TestPersonCreateCLIIfExistsPublishBoundary(t *testing.T) {
	for _, status := range []string{"created", "matched_existing"} {
		t.Run(status, func(t *testing.T) {
			var publishes int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/health":
					_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.50.0"}`))
				case "/api/v1/people/create":
					var input map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
					assert.Equal(t, "return", input["if_exists"])
					code, basis := http.StatusCreated, `[]`
					if status == "matched_existing" {
						code = http.StatusOK
						basis = `[{"kind":"email","value":"alex@example.test","matched":"contact_point","person_id":12,"participant_id":null}]`
					}
					w.WriteHeader(code)
					_, _ = fmt.Fprintf(w, `{"id":12,"display_name":"Alex Example","vcard_uid":"existing-person","revision":1,"participant_ids":[],"created_at":"2026-10-08T12:00:00Z","updated_at":"2026-10-08T12:00:00Z","ensure":{"status":%q,"match_basis":%s,"unapplied_contacts":[]}}`, status, basis)
				case "/api/v1/carddav/publications/12":
					publishes++
					w.WriteHeader(http.StatusConflict)
					_, _ = w.Write([]byte(`{"error":"carddav_inference_review_required","message":"Preview and approve publication"}`))
				default:
					assert.Fail(t, "unexpected path", r.URL.Path)
				}
			}))
			t.Cleanup(server.Close)
			home := t.TempDir()
			ctx := withStoreResolverConfig(t, &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}, Remote: config.RemoteConfig{URL: server.URL, APIKey: "test-key", AllowInsecure: true}})
			cmd := newPersonCreateCommand()
			silenceUsageInRunE(cmd)
			cmd.SetContext(ctx)
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(io.Discard)
			cmd.SetArgs([]string{"--name", "Ignored Name", "--email", "alex@example.test", "--if-exists", "return", "--publish"})
			err := cmd.Execute()
			if status == "created" {
				require.ErrorContains(t, err, "publication failed")
				assert.Equal(t, 1, publishes)
			} else {
				require.NoError(t, err)
				assert.Equal(t, "Matched person 12 (Alex Example) by email alex@example.test\n", out.String())
				assert.Zero(t, publishes)
			}
		})
	}
}

func TestPersonCreateCLIIfExistsProduction(t *testing.T) {
	st := testutil.NewTestStore(t)
	srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "test-key"}}, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)})
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	home := t.TempDir()
	ctx := withStoreResolverConfig(t, &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}, Remote: config.RemoteConfig{URL: server.URL, APIKey: "test-key", AllowInsecure: true}})
	run := func(email string) (map[string]any, error) {
		command := newPersonCreateCommand()
		silenceUsageInRunE(command)
		command.SetContext(ctx)
		var out bytes.Buffer
		command.SetOut(&out)
		command.SetErr(io.Discard)
		command.SetArgs([]string{"--name", "Alex Example", "--email", email, "--if-exists", "return", "--json"})
		err := command.Execute()
		var body map[string]any
		require.NoError(t, json.Unmarshal(out.Bytes(), &body))
		return body, err
	}
	first, err := run("alex@example.test")
	require.NoError(t, err)
	assert.Equal(t, "created", first["ensure"].(map[string]any)["status"])
	second, err := run("alex@example.test")
	require.NoError(t, err)
	assert.Equal(t, first["id"], second["id"])
	assert.Equal(t, "matched_existing", second["ensure"].(map[string]any)["status"])
	_, err = st.EnsureParticipant("observed@example.test", "Observed Example", "example.test")
	require.NoError(t, err)
	conflict, err := run("observed@example.test")
	require.ErrorContains(t, err, "person_matches_participant")
	assert.Equal(t, "person_matches_participant", conflict["error"])
	assert.Contains(t, conflict["details"], "next")
}

func TestPersonCreateCLIIfExistsRejectsInvalid(t *testing.T) {
	cmd := newPersonCreateCommand()
	cmd.SetContext(t.Context())
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--name", "Alex Example", "--if-exists", "bogus"})
	require.ErrorContains(t, cmd.Execute(), "if_exists must be error or return")
	t.Run("older daemon", func(t *testing.T) {
		var creates int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/health":
				_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.49.0"}`))
			case "/api/v1/people/create":
				creates++
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"bad_request","message":"Invalid person request: unknown object member name if_exists"}`))
			default:
				assert.Fail(t, "unexpected path", r.URL.Path)
			}
		}))
		t.Cleanup(server.Close)
		cmd := newPersonCreateCommand()
		cmd.SetContext(configureRemoteDaemonForTest(t, server.URL))
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs([]string{"--name", "Alex Example", "--if-exists", "return"})
		err := cmd.Execute()
		require.ErrorContains(t, err, "upgrade msgvault")
		assert.ErrorContains(t, err, "does not support person create --if-exists return")
		assert.Zero(t, creates)
	})
}
