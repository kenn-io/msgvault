package cmd

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/forge"
	"go.kenn.io/msgvault/internal/testutil"
)

const rotationForge = "fakeforge"

// rotationAccountForge is one account on a fake forge whose discovery pauses
// at a rate limit while limited is set.
type rotationAccountForge struct {
	id      string
	limited bool
	synced  *[]string
}

func (f *rotationAccountForge) Name() string             { return rotationForge }
func (f *rotationAccountForge) Instance() forge.Instance { return "forge.example.com" }
func (f *rotationAccountForge) Account(context.Context) (forge.Account, error) {
	*f.synced = append(*f.synced, f.id)
	return forge.Account{Kind: forge.AuthorAccount, ID: f.id, Login: "user" + f.id}, nil
}

func (f *rotationAccountForge) Discover(context.Context, []byte, forge.OwnerFilter, forge.KnownThreads,
	func([]forge.ThreadRef, []byte) error,
) error {
	if f.limited {
		return &forge.RateLimitError{Resource: "api", Reset: time.Now().Add(time.Hour)}
	}
	return nil
}

func (f *rotationAccountForge) Owner(context.Context, forge.ThreadRef) (string, error) { return "", nil }

func (f *rotationAccountForge) Fetch(context.Context, forge.ThreadRef, []byte,
	func(forge.ThreadHead, []forge.Post, []byte) error,
) error {
	return nil
}

func TestScheduledForgeSyncContinuesWithTheNextAccountAfterAPause(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := testConfigValue()
	cfg.Data.DataDir = t.TempDir()
	cfg.Analytics.AutoBuildCache = false
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})

	var synced []string
	limited := map[string]bool{"1": true}
	fs := &forgeSource{
		name:   rotationForge,
		label:  "Fake forge",
		config: func(*config.Config) config.ForgeConfig { return config.ForgeConfig{} },
		open: func(_ *url.URL, token string) forge.Forge {
			return &rotationAccountForge{id: token, limited: limited[token], synced: &synced}
		},
	}
	for _, id := range []string{"1", "2"} {
		_, err := st.GetOrCreateSourceContext(t.Context(), rotationForge, forge.SourceIdentifier("forge.example.com", id))
		require.NoError(err)
		require.NoError(forge.SaveToken(cfg.TokensDir(), rotationForge, forge.Token{
			AccessToken: id, BaseURL: "https://forge.example.com", Instance: "forge.example.com", AccountID: id, Login: "user" + id,
		}))
	}
	t.Cleanup(func() { forgeRotation.resumeAt(rotationForge, "") })

	require.NoError(fs.runScheduled(ctx, st))
	assert.Equal([]string{"1"}, synced, "a paused account ends the run")
	require.NoError(fs.runScheduled(ctx, st))
	assert.Equal([]string{"1", "2", "1"}, synced, "the next run starts with the account after it")
}

func TestForgeSyncPicksTheAccountByLoginOrIdentifier(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	tokens := t.TempDir()
	fs := &forgeSource{name: rotationForge, label: "Fake forge"}
	for _, id := range []string{"1", "2"} {
		_, err := st.GetOrCreateSourceContext(t.Context(), rotationForge, forge.SourceIdentifier("forge.example.com", id))
		require.NoError(err)
		require.NoError(forge.SaveToken(tokens, rotationForge, forge.Token{
			AccessToken: id, BaseURL: "https://forge.example.com", Instance: "forge.example.com", AccountID: id, Login: "User" + id,
		}))
	}

	all, err := fs.sources(t.Context(), st, tokens, "")
	require.NoError(err)
	assert.Len(all, 2)
	byLogin, err := fs.sources(t.Context(), st, tokens, "user2")
	require.NoError(err)
	require.Len(byLogin, 1)
	assert.Equal("forge.example.com:user:2", byLogin[0].Identifier, "logins compare without case")
	byIdentifier, err := fs.sources(t.Context(), st, tokens, "forge.example.com:user:1")
	require.NoError(err)
	require.Len(byIdentifier, 1)
	_, err = fs.sources(t.Context(), st, tokens, "nobody")
	require.ErrorContains(err, `"nobody" is not added`)
}
