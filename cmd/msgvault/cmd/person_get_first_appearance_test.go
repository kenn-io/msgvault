package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonGetReturnsPersonWhenFirstAppearanceProbeFails(t *testing.T) {
	check := assert.New(t)
	req := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/people/7":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":7,"vcard_uid":"person-7","revision":2,"participant_ids":[],"created_at":"2026-07-29T12:00:00Z","updated_at":"2026-07-29T12:00:00Z"}`))
		case "/api/v1/health":
			http.Error(w, "unavailable", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	ctx := withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
	savedJSON := personJSON
	personJSON = true
	t.Cleanup(func() { personJSON = savedJSON })
	get := &cobra.Command{Use: personGetCmd.Use, Args: personGetCmd.Args, RunE: personGetCmd.RunE}
	get.Flags().String("vcard-uid", "", "")
	var got bytes.Buffer
	get.SetOut(&got)
	get.SetArgs([]string{"7"})
	req.NoError(get.ExecuteContext(ctx))
	var person map[string]any
	req.NoError(json.Unmarshal(got.Bytes(), &person))
	check.InDelta(7, person["id"], 0)
	check.NotContains(person, "contact_state")
}

func TestPersonGetFirstAppearanceRealProjection(t *testing.T) {
	check := assert.New(t)
	req := require.New(t)
	f := storetest.New(t)
	req.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
	owner := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
	sender := f.EnsureParticipant("sender@example.test", "Sender Example", "example.test")
	senderPerson, _, err := f.Store.CreatePersonFromParticipant(sender)
	req.NoError(err)
	_, err = f.Store.UpdatePersonDisplayNameContext(t.Context(), senderPerson.ID, senderPerson.Revision, new("Saved Sender"))
	req.NoError(err)
	target := f.EnsureParticipant("target@example.test", "Target Example", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(target)
	req.NoError(err)
	at := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	id := f.NewMessage().WithSourceMessageID("intro-cli").WithSentAt(at).Create(t, f.Store)
	req.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, []string{"Sender Example"}))
	req.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{owner, target}, []string{"Owner", "Target Example"}))
	projector, err := activity.NewProjector(f.Store, activity.Options{Timezone: "UTC", BatchSize: 10, MaxDirectCounterparts: 25})
	req.NoError(err)
	_, err = projector.RunOnce(t.Context())
	req.NoError(err)
	srv := api.NewServer(&config.Config{}, &storeAPIAdapter{store: f.Store}, nil, slog.New(slog.DiscardHandler))
	var oldDaemon, failState, malformedState, emptyState atomic.Bool
	router := srv.Router()
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if oldDaemon.Load() && r.URL.Path == "/api/v1/health" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"5.0.0"}`))
			return
		}
		if failState.Load() && r.URL.Path == fmt.Sprintf("/api/v1/people/%d/contact-state", person.ID) {
			http.Error(w, "unavailable", http.StatusInternalServerError)
			return
		}
		if emptyState.Load() && r.URL.Path == fmt.Sprintf("/api/v1/people/%d/contact-state", person.ID) {
			w.Header().Set("Content-Type", "application/json")
			return
		}
		if malformedState.Load() && r.URL.Path == fmt.Sprintf("/api/v1/people/%d/contact-state", person.ID) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{"))
			return
		}
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	ctx := withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: httpServer.URL, AllowInsecure: true}})
	savedJSON := personJSON
	personJSON = true
	t.Cleanup(func() { personJSON = savedJSON })
	for phase := range 2 {
		if phase == 1 {
			id = f.NewMessage().WithSourceMessageID("older-intro-cli").WithSentAt(at.Add(-time.Hour)).Create(t, f.Store)
			req.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, []string{"Sender Example"}))
			req.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{owner, target}, []string{"Owner", "Target Example"}))
			_, err = projector.RunOnce(t.Context())
			req.NoError(err)
		}
		wantRef := fmt.Sprintf("message:%d", id)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("%s/api/v1/people/%d/contact-state", httpServer.URL, person.ID), nil)
		req.NoError(err)
		response, err := http.DefaultClient.Do(request)
		req.NoError(err)
		req.Equal(http.StatusOK, response.StatusCode)
		var state map[string]any
		req.NoError(json.NewDecoder(response.Body).Decode(&state))
		req.NoError(response.Body.Close())
		check.Equal(wantRef, state["first_seen_ref"])
		check.Equal("email_intro", state["first_contact_kind"])
		for _, args := range [][]string{{strconv.FormatInt(person.ID, 10)}, {"--vcard-uid", person.VCardUID}} {
			get := &cobra.Command{Use: personGetCmd.Use, Args: personGetCmd.Args, RunE: personGetCmd.RunE}
			get.Flags().String("vcard-uid", "", "")
			var got bytes.Buffer
			get.SetOut(&got)
			get.SetArgs(args)
			req.NoError(get.ExecuteContext(ctx))
			var profile map[string]any
			req.NoError(json.Unmarshal(got.Bytes(), &profile))
			contact, ok := profile["contact_state"].(map[string]any)
			req.True(ok)
			check.Equal(wantRef, contact["first_seen_ref"])
		}
	}
	for _, scenario := range []struct {
		name                        string
		old, fail, malformed, empty bool
	}{{"older daemon", true, false, false, false}, {"optional subresource unavailable", false, true, false, false}, {"malformed contact state", false, false, true, false}, {"empty contact state", false, false, false, true}} {
		t.Run(scenario.name, func(t *testing.T) {
			check := assert.New(t)
			req := require.New(t)
			oldDaemon.Store(scenario.old)
			failState.Store(scenario.fail)
			malformedState.Store(scenario.malformed)
			emptyState.Store(scenario.empty)
			for _, args := range [][]string{{strconv.FormatInt(person.ID, 10)}, {"--vcard-uid", person.VCardUID}} {
				get := &cobra.Command{Use: personGetCmd.Use, Args: personGetCmd.Args, RunE: personGetCmd.RunE}
				get.Flags().String("vcard-uid", "", "")
				var got bytes.Buffer
				get.SetOut(&got)
				get.SetArgs(args)
				err := get.ExecuteContext(ctx)
				req.NoError(err)
				var profile map[string]any
				req.NoError(json.Unmarshal(got.Bytes(), &profile))
				check.InDelta(float64(person.ID), profile["id"], 0)
				check.NotContains(profile, "contact_state")
			}
		})
	}
}
