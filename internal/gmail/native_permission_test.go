package gmail_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/oauth"
	"golang.org/x/oauth2"
)

func TestGmailNativeMessagePermissionUsesSavedGrant(t *testing.T) {
	for _, tc := range []struct {
		name        string
		scopes      []string
		known, read bool
	}{
		{"legacy_unknown", nil, false, false},
		{"labels_only", []string{oauth.ScopeGmailLabels}, true, false},
		{"metadata_only", []string{oauth.ScopeGmailMetadata}, true, false},
		{"compose_only", []string{oauth.ScopeGmailCompose}, true, false},
		{"read_only", []string{oauth.ScopeGmailReadonly}, true, true},
		{"modify", []string{oauth.ScopeGmailModify}, true, true},
		{"full", []string{oauth.ScopeGmailFull}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			granted := append([]string(nil), tc.scopes...)
			client := gmail.NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "synthetic-token"}), gmail.WithGrantedScopes(granted))
			if len(granted) > 0 {
				granted[0] = "https://example.net/unrelated"
			}
			read, known := client.NativeMessageReadPermission()
			assert.Equal(t, tc.read, read)
			assert.Equal(t, tc.known, known)
		})
	}
}
