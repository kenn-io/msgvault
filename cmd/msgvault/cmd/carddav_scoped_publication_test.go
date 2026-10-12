package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type publicationBrokenOutput struct{ err error }

func (w publicationBrokenOutput) Write([]byte) (int, error) { return 0, w.err }

func TestScopedCardDAVCLIRecoversOriginalReceiptAfterLostOutput(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	var mu sync.Mutex
	var body []byte
	var puts, gets int
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("ETag", `"synthetic-cli"`)
		switch r.Method {
		case http.MethodPut:
			puts++
			data, err := io.ReadAll(r.Body)
			assertions.NoError(err)
			body = data
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			gets++
			w.Header().Set("Content-Type", "text/vcard")
			_, err := w.Write(body)
			assertions.NoError(err)
		default:
			assertions.Fail("only the native mapped PUT and GET are expected")
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(provider.Close)
	providerURL, err := url.Parse(provider.URL)
	requirements.NoError(err)
	origin, err := url.Parse("http://203.0.113.9:" + providerURL.Port())
	requirements.NoError(err)
	client, err := carddav.NewClient(carddav.ClientOptions{CredentialOrigin: origin,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, provider.Listener.Addr().String())
		}})
	requirements.NoError(err)
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: origin.String() + "/dav", Username: "synthetic-cli", PrincipalURL: origin.String() + "/principal/", HomeURL: origin.String() + "/books/",
		Books: []store.CardDAVDiscoveredBook{{CanonicalURL: origin.String() + "/books/synthetic/", CanCreate: new(true), CanUpdate: new(true), CanDelete: new(true)}},
	})
	requirements.NoError(err)
	requirements.Len(books, 1)
	requirements.NoError(st.SetCardDAVBookRolesContext(t.Context(), books[0].ID, store.CardDAVBookRoles{IsWriteTarget: true, IsSubscribed: true, IsLookupSource: true}))
	participant, err := st.EnsureParticipant("cli-receipt@example.test", "Synthetic CLI Person", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	service := carddav.NewService(st, client)
	requirements.NoError(service.PublishPerson(t.Context(), person.ID))
	_, err = st.AppendPersonNoteContext(t.Context(), store.PersonNoteAppendInput{PersonID: person.ID, Text: "Synthetic reviewed CLI correction", Source: store.ProvenanceUser})
	requirements.NoError(err)
	allow := func(context.Context, *store.IdentityGrantSelection) error { return nil }
	preview, err := service.PreviewPublicationAuthorized(t.Context(), person.ID, allow)
	requirements.NoError(err)
	mu.Lock()
	puts, gets = 0, 0
	mu.Unlock()
	const key = "synthetic-cli-original-request"
	receipt, err := service.PublishReviewedPersonWithReceipt(t.Context(), person.ID, preview.ApprovalToken, "owner", key, allow)
	requirements.NoError(err)
	requirements.Equal("verified", receipt.State)
	// A new daemon exposes the committed archive without the original provider
	// client. CLI approval and recovery must consult the original receipt first.
	const apiKey = "synthetic-cli-owner-key"
	server := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: apiKey}}, Store: st, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() {
		assertions.NoError(server.Shutdown(context.Background()))
	})
	daemon := httptest.NewServer(server.Router())
	t.Cleanup(daemon.Close)
	home := t.TempDir()
	ctx := withStoreResolverConfig(t, &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}, Remote: config.RemoteConfig{URL: daemon.URL, APIKey: apiKey, AllowInsecure: true}})
	args := []string{strconv.FormatInt(person.ID, 10), "--scoped", "--approve", preview.ApprovalToken, "--idempotency-key", key}
	broken := errors.New("synthetic lost stdout")
	command := newPersonCardDAVCommand("publish", true)
	command.SetContext(ctx)
	command.SetOut(publicationBrokenOutput{broken})
	command.SetArgs(args)
	requirements.ErrorIs(command.Execute(), broken)
	for _, reconcile := range []bool{false, true} {
		t.Run(fmt.Sprintf("reconcile=%t", reconcile), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			command := newPersonCardDAVCommand("publish", true)
			command.SetContext(ctx)
			var output bytes.Buffer
			command.SetOut(&output)
			invocation := append([]string(nil), args...)
			if reconcile {
				invocation = append(invocation, "--reconcile")
			}
			command.SetArgs(invocation)
			requirements.NoError(command.Execute())
			var saved struct {
				Receipt *store.CardDAVPublicationReceipt `json:"receipt"`
			}
			requirements.NoError(json.Unmarshal(output.Bytes(), &saved))
			requirements.NotNil(saved.Receipt)
			assertions.Equal(receipt, saved.Receipt)
		})
	}
	mu.Lock()
	defer mu.Unlock()
	assertions.Equal(1, puts, "lost output and recovery never replay the original PUT")
	assertions.Equal(1, gets, "settled receipts need no further provider observations")
}
