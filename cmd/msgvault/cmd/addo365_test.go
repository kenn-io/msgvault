package cmd

import (
	"database/sql"
	"testing"

	imapclient "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
)

func TestIsMicrosoftIMAPSource(t *testing.T) {
	xoauth2Config := func(username string) string {
		cfg := &imapclient.Config{
			Host:       "outlook.office365.com",
			Port:       993,
			TLS:        true,
			Username:   username,
			AuthMethod: imapclient.AuthXOAuth2,
		}
		j, _ := cfg.ToJSON()
		return j
	}
	passwordConfig := func(username string) string {
		cfg := &imapclient.Config{
			Host:     "imap.example.com",
			Port:     993,
			TLS:      true,
			Username: username,
		}
		j, _ := cfg.ToJSON()
		return j
	}

	tests := []struct {
		name  string
		src   *store.Source
		email string
		want  bool
	}{
		{
			name: "microsoft xoauth2 matching username",
			src: &store.Source{
				SourceType: "imap",
				SyncConfig: sql.NullString{Valid: true, String: xoauth2Config("user@company.com")},
			},
			email: "user@company.com",
			want:  true,
		},
		{
			name: "microsoft xoauth2 username case-insensitive",
			src: &store.Source{
				SourceType: "imap",
				SyncConfig: sql.NullString{Valid: true, String: xoauth2Config("User@Company.com")},
			},
			email: "user@company.com",
			want:  true,
		},
		{
			name: "non-microsoft password-auth source same display name",
			src: &store.Source{
				SourceType: "imap",
				SyncConfig: sql.NullString{Valid: true, String: passwordConfig("user@company.com")},
			},
			email: "user@company.com",
			want:  false,
		},
		{
			name: "xoauth2 but different username",
			src: &store.Source{
				SourceType: "imap",
				SyncConfig: sql.NullString{Valid: true, String: xoauth2Config("other@company.com")},
			},
			email: "user@company.com",
			want:  false,
		},
		{
			name: "no sync_config",
			src: &store.Source{
				SourceType: "imap",
				SyncConfig: sql.NullString{Valid: false},
			},
			email: "user@company.com",
			want:  false,
		},
		{
			name: "invalid sync_config json",
			src: &store.Source{
				SourceType: "imap",
				SyncConfig: sql.NullString{Valid: true, String: "not-json"},
			},
			email: "user@company.com",
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isMicrosoftIMAPSource(tt.src, tt.email)
			if got != tt.want {
				t.Errorf("isMicrosoftIMAPSource() = %v, want %v", got, tt.want)
			}
		})
	}
}
