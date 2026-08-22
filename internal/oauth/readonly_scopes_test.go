package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

const driveReadonlyScope = "https://www.googleapis.com/auth/drive.readonly"

func TestGmailWriteScopeHelpers(t *testing.T) {
	tests := []struct {
		name           string
		scopes         []string
		wantWrite      bool
		wantAnyGmail   bool
		wantNarrowed   bool
		wantWithoutSet []string
	}{
		{
			name:           "empty grant",
			scopes:         nil,
			wantWithoutSet: []string{},
		},
		{
			name:           "read-only gmail is a narrowed grant",
			scopes:         []string{ScopeGmailReadonly},
			wantAnyGmail:   true,
			wantNarrowed:   true,
			wantWithoutSet: []string{ScopeGmailReadonly},
		},
		{
			name:           "modify is write access",
			scopes:         Scopes,
			wantWrite:      true,
			wantAnyGmail:   true,
			wantWithoutSet: []string{ScopeGmailReadonly},
		},
		{
			// The scope a check written against gmail.modify alone misses.
			name:           "full access is write access",
			scopes:         []string{ScopeGmailFull},
			wantWrite:      true,
			wantAnyGmail:   true,
			wantWithoutSet: []string{},
		},
		{
			name:           "non-gmail grants are not a narrowed gmail grant",
			scopes:         []string{ScopeCalendarReadonly, driveReadonlyScope},
			wantWithoutSet: []string{ScopeCalendarReadonly, driveReadonlyScope},
		},
		{
			name:           "narrowing keeps non-gmail grants in order",
			scopes:         []string{ScopeCalendarReadonly, ScopeGmailModify, driveReadonlyScope, ScopeGmailFull},
			wantWrite:      true,
			wantAnyGmail:   true,
			wantWithoutSet: []string{ScopeCalendarReadonly, driveReadonlyScope},
		},
		{
			// msgvault never requests gmail.send, but a token it is asked to
			// reuse may carry it, and it confers the ability to act as the
			// user. A set that stopped at modify and full would call this
			// grant read-only.
			name:           "send is write access",
			scopes:         []string{ScopeGmailReadonly, ScopeGmailSend},
			wantWrite:      true,
			wantAnyGmail:   true,
			wantWithoutSet: []string{ScopeGmailReadonly},
		},
		{
			name: "every unrequested write scope is stripped",
			scopes: []string{
				ScopeGmailReadonly,
				ScopeGmailCompose,
				ScopeGmailInsert,
				ScopeGmailLabels,
				ScopeGmailSettingsBasic,
				ScopeGmailSettingsSharing,
				ScopeCalendarReadonly,
			},
			wantWrite:      true,
			wantAnyGmail:   true,
			wantWithoutSet: []string{ScopeGmailReadonly, ScopeCalendarReadonly},
		},
		{
			// gmail.metadata reads headers and labels without bodies. It is
			// read-only and must survive narrowing.
			name:           "metadata is a narrowed gmail grant",
			scopes:         []string{ScopeGmailMetadata},
			wantAnyGmail:   true,
			wantNarrowed:   true,
			wantWithoutSet: []string{ScopeGmailMetadata},
		},
		{
			// The reason classification allow-lists the read scopes rather
			// than deny-listing the write ones. This scope is not in
			// ScopesGmailWrite and nothing in the repo mentions it, yet it
			// can send mail as the user. A deny-list would call this grant
			// read-only; an allow-list calls it write.
			name:           "an unlisted gmail scope counts as write",
			scopes:         []string{ScopeGmailReadonly, "https://www.googleapis.com/auth/gmail.addons.current.action.compose"},
			wantWrite:      true,
			wantAnyGmail:   true,
			wantWithoutSet: []string{ScopeGmailReadonly},
		},
		{
			// Add-on read scopes are genuinely read-only and must survive.
			name:           "add-on read scopes are not write",
			scopes:         []string{"https://www.googleapis.com/auth/gmail.addons.current.message.readonly"},
			wantAnyGmail:   true,
			wantNarrowed:   true,
			wantWithoutSet: []string{"https://www.googleapis.com/auth/gmail.addons.current.message.readonly"},
		},
		{
			// A future Gmail scope nobody has heard of. Fails closed.
			name:           "an entirely unknown gmail scope counts as write",
			scopes:         []string{"https://www.googleapis.com/auth/gmail.somethingnew"},
			wantWrite:      true,
			wantAnyGmail:   true,
			wantWithoutSet: []string{},
		},
		{
			// Non-Gmail scopes must not be swept up by the prefix match.
			name:           "non-gmail scopes are never gmail write",
			scopes:         []string{ScopeCalendarReadonly, driveReadonlyScope, "https://www.googleapis.com/auth/gmailish.fake"},
			wantWithoutSet: []string{ScopeCalendarReadonly, driveReadonlyScope, "https://www.googleapis.com/auth/gmailish.fake"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)

			assert.Equal(tt.wantWrite, HasGmailWriteScope(tt.scopes), "HasGmailWriteScope")
			assert.Equal(tt.wantAnyGmail, HasAnyGmailScope(tt.scopes), "HasAnyGmailScope")
			assert.Equal(tt.wantNarrowed, IsNarrowedGmailGrant(tt.scopes), "IsNarrowedGmailGrant")
			assert.Equal(tt.wantWithoutSet, WithoutGmailWriteScopes(tt.scopes), "WithoutGmailWriteScopes")
		})
	}
}

// TestTokenIssuedByDifferentClient pins the asymmetry with TokenMatchesClient.
// Callers waive the read-only refusal on the strength of "this is a different
// client", so uncertainty must not read as a yes: a token with no recorded
// client_id, or none at all, is unknown provenance rather than evidence.
func TestTokenIssuedByDifferentClient(t *testing.T) {
	const email = "user@example.com"

	tests := []struct {
		name      string
		writeFile bool
		clientID  string
		want      bool
	}{
		{
			name:      "same client",
			writeFile: true,
			clientID:  "client-a",
			want:      false,
		},
		{
			name:      "different client",
			writeFile: true,
			clientID:  "client-b",
			want:      true,
		},
		{
			// Legacy token: provenance unknown, so not evidence of a
			// different client.
			name:      "no recorded client id",
			writeFile: true,
			clientID:  "",
			want:      false,
		},
		{
			name:      "no token at all",
			writeFile: false,
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := setupTestManager(t, Scopes)
			mgr.config.ClientID = "client-a"

			if tt.writeFile {
				tf := tokenFile{
					AccessToken: "t",
					TokenType:   "Bearer",
					Scopes:      Scopes,
					ClientID:    tt.clientID,
				}
				data, err := json.Marshal(tf)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(
					filepath.Join(mgr.tokensDir, email+".json"), data, 0600))
			}

			assert.Equal(t, tt.want, mgr.TokenIssuedByDifferentClient(email))
		})
	}
}

// TestScopesWithPreservedGrants_DoesNotRewidenNarrowedGrant guards the
// incidental re-authorization paths — an expired token, or adding Calendar.
// They union the caller's required scopes with what the account already holds,
// which would hand Gmail write access back to an account deliberately narrowed
// with `add-account --readonly`.
func TestScopesWithPreservedGrants_DoesNotRewidenNarrowedGrant(t *testing.T) {
	tests := []struct {
		name     string
		required []string
		granted  []string
		want     []string
	}{
		{
			name:     "narrowed gmail grant stays narrow",
			required: Scopes,
			granted:  []string{ScopeGmailReadonly},
			want:     []string{ScopeGmailReadonly},
		},
		{
			name:     "narrowed gmail grant keeps calendar and stays narrow",
			required: ScopesGmailCalendar,
			granted:  []string{ScopeGmailReadonly, ScopeCalendarReadonly},
			want:     []string{ScopeGmailReadonly, ScopeCalendarReadonly},
		},
		{
			name:     "write-capable grant is unchanged",
			required: Scopes,
			granted:  []string{ScopeGmailReadonly, ScopeGmailModify, ScopeCalendarReadonly},
			want:     []string{ScopeGmailReadonly, ScopeGmailModify, ScopeCalendarReadonly},
		},
		{
			name:     "full access grant is unchanged and keeps its own scope",
			required: Scopes,
			granted:  []string{ScopeGmailFull},
			want:     []string{ScopeGmailReadonly, ScopeGmailModify, ScopeGmailFull},
		},
		{
			// No Gmail scopes at all is not a narrowed grant; this account is
			// getting Gmail for the first time and must widen as before.
			name:     "grant with no gmail scopes still gains gmail",
			required: Scopes,
			granted:  []string{ScopeCalendarReadonly},
			want:     []string{ScopeGmailReadonly, ScopeGmailModify, ScopeCalendarReadonly},
		},
		{
			// Legacy tokens carry no scope metadata, so there is nothing to
			// read as narrowed and behavior is unchanged.
			name:     "legacy token with no recorded scopes is unchanged",
			required: Scopes,
			granted:  nil,
			want:     []string{ScopeGmailReadonly, ScopeGmailModify},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.ElementsMatch(t, tt.want, scopesWithPreservedGrants(tt.required, tt.granted))
		})
	}
}

// TestAuthorize_NarrowedRequestPersistsNarrowedGrant is the end-to-end check
// that a narrowed request actually lands as a narrowed grant on disk, with
// non-Gmail scopes intact. Everything above reasons about scope lists; this
// runs the real Authorize path and reads back the stored token.
func TestAuthorize_NarrowedRequestPersistsNarrowedGrant(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const email = "user@example.com"
	narrowed := []string{ScopeGmailReadonly, ScopeCalendarReadonly}

	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"emailAddress": %q}`, email)
		}))
	defer srv.Close()

	mgr := setupTestManager(t, narrowed)
	mgr.profileURL = srv.URL

	// The account starts out write-capable with Calendar attached. add-account
	// now refuses to narrow such an account, because the old grant would stay
	// live at Google — but that is a policy decision in the command layer. This
	// test covers the layer below it: when a narrowed request IS made, the
	// manager must record exactly what came back.
	writeTokenFile(t, mgr, email, oauth2.Token{
		AccessToken: "old-token",
		TokenType:   "Bearer",
		Expiry:      time.Now().Add(time.Hour),
	}, []string{ScopeGmailReadonly, ScopeGmailModify, ScopeCalendarReadonly})

	// Google echoes the granted scopes back on the token exchange.
	mgr.browserFlowFn = func(_ context.Context, _ string, _ bool) (*oauth2.Token, error) {
		token := &oauth2.Token{
			AccessToken: "narrowed-token",
			TokenType:   "Bearer",
			Expiry:      time.Now().Add(time.Hour),
		}
		return token.WithExtra(map[string]any{
			"scope": ScopeGmailReadonly + " " + ScopeCalendarReadonly,
		}), nil
	}

	require.NoError(mgr.Authorize(context.Background(), email), "Authorize")

	assert.ElementsMatch(narrowed, mgr.GrantedScopes(email),
		"stored grant must be exactly what was requested")
	assert.False(HasGmailWriteScope(mgr.GrantedScopes(email)),
		"narrowing must remove Gmail write access")
	assert.True(mgr.HasScope(email, ScopeCalendarReadonly),
		"narrowing must not discard Calendar")
}

// TestAuthorize_StillRejectsUnderDeliveredScopes confirms the narrowing work
// did not weaken the existing guard: a token that comes back with fewer scopes
// than requested is still refused.
func TestAuthorize_StillRejectsUnderDeliveredScopes(t *testing.T) {
	const email = "user@example.com"

	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"emailAddress": %q}`, email)
		}))
	defer srv.Close()

	mgr := setupTestManager(t, Scopes)
	mgr.profileURL = srv.URL
	mgr.browserFlowFn = func(_ context.Context, _ string, _ bool) (*oauth2.Token, error) {
		token := &oauth2.Token{
			AccessToken: "partial-token",
			TokenType:   "Bearer",
			Expiry:      time.Now().Add(time.Hour),
		}
		return token.WithExtra(map[string]any{"scope": ScopeGmailReadonly}), nil
	}

	err := mgr.Authorize(context.Background(), email)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing required OAuth scopes")
	assert.Contains(t, err.Error(), ScopeGmailModify)
}
