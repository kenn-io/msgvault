package imap

import (
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func TestIdentifier(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "TLS",
			cfg:  Config{Host: "imap.example.com", Port: 993, TLS: true, Username: "user@example.com"},
			want: "imaps://user@example.com@imap.example.com:993",
		},
		{
			name: "STARTTLS",
			cfg:  Config{Host: "mail.example.com", Port: 143, STARTTLS: true, Username: "user@example.com"},
			want: "imap+starttls://user@example.com@mail.example.com:143",
		},
		{
			name: "Plaintext",
			cfg:  Config{Host: "mail.example.com", Port: 143, Username: "user@example.com"},
			want: "imap://user@example.com@mail.example.com:143",
		},
		{
			name: "TLS default port",
			cfg:  Config{Host: "imap.example.com", TLS: true, Username: "user"},
			want: "imaps://user@imap.example.com:993",
		},
		{
			name: "Non-TLS default port",
			cfg:  Config{Host: "mail.example.com", Username: "user"},
			want: "imap://user@mail.example.com:143",
		},
		{
			name: "IPv6 host unbracketed",
			cfg:  Config{Host: "::1", Port: 1993, Username: "user"},
			want: "imap://user@[::1]:1993",
		},
		{
			name: "IPv6 host bracketed",
			cfg:  Config{Host: "[::1]", Port: 1993, Username: "user"},
			want: "imap://user@[::1]:1993",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.Identifier()
			assertpkg.Equal(t, tt.want, got, "Identifier()")
		})
	}
}

func TestAddr_IPv6(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "unbracketed",
			cfg:  Config{Host: "::1", Port: 1993},
			want: "[::1]:1993",
		},
		{
			name: "bracketed",
			cfg:  Config{Host: "[::1]", Port: 1993},
			want: "[::1]:1993",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.Addr()
			assertpkg.Equal(t, tt.want, got, "Addr()")
		})
	}
}

func TestIdentifier_STARTTLSDistinctFromPlaintext(t *testing.T) {
	starttls := Config{
		Host: "mail.example.com", Port: 143,
		STARTTLS: true, Username: "user@example.com",
	}
	plain := Config{
		Host: "mail.example.com", Port: 143,
		Username: "user@example.com",
	}
	assertpkg.NotEqual(t, plain.Identifier(), starttls.Identifier(),
		"STARTTLS and plaintext should have distinct identifiers")
}

func TestParseIdentifier_RoundTrip(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{
			name: "TLS",
			cfg:  Config{Host: "imap.example.com", Port: 993, TLS: true, Username: "user@example.com"},
		},
		{
			name: "STARTTLS",
			cfg:  Config{Host: "mail.example.com", Port: 143, STARTTLS: true, Username: "user@example.com"},
		},
		{
			name: "Plaintext",
			cfg:  Config{Host: "mail.example.com", Port: 143, Username: "user@example.com"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assertpkg.New(t)
			id := tt.cfg.Identifier()
			parsed, err := ParseIdentifier(id)
			requirepkg.NoError(t, err, "ParseIdentifier(%q)", id)
			assert.Equal(tt.cfg.Host, parsed.Host, "Host")
			assert.Equal(tt.cfg.Port, parsed.Port, "Port")
			assert.Equal(tt.cfg.TLS, parsed.TLS, "TLS")
			assert.Equal(tt.cfg.STARTTLS, parsed.STARTTLS, "STARTTLS")
			assert.Equal(tt.cfg.Username, parsed.Username, "Username")
		})
	}
}

func TestParseIdentifier_InvalidScheme(t *testing.T) {
	_, err := ParseIdentifier("pop3://user@host:110")
	assertpkg.Error(t, err, "expected error for unsupported scheme")
}

func TestConfigAuthMethod_DefaultsToPassword(t *testing.T) {
	// Existing JSON without auth_method should default to password
	cfg, err := ConfigFromJSON(`{"host":"imap.example.com","port":993,"tls":true,"username":"user"}`)
	requirepkg.NoError(t, err)
	if cfg.AuthMethod != "" {
		assertpkg.Equal(t, AuthPassword, cfg.AuthMethod, "AuthMethod should be empty or %q", AuthPassword)
	}
	assertpkg.Equal(t, AuthPassword, cfg.EffectiveAuthMethod(), "EffectiveAuthMethod()")
}

func TestConfigAuthMethod_XOAuth2(t *testing.T) {
	cfg, err := ConfigFromJSON(`{"host":"outlook.office365.com","port":993,"tls":true,"username":"user@company.com","auth_method":"xoauth2"}`)
	requirepkg.NoError(t, err)
	assertpkg.Equal(t, AuthXOAuth2, cfg.AuthMethod, "AuthMethod")
	assertpkg.Equal(t, AuthXOAuth2, cfg.EffectiveAuthMethod(), "EffectiveAuthMethod()")
}
