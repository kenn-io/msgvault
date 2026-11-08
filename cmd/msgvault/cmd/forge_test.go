package cmd

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/forge"
	"go.kenn.io/msgvault/internal/store"
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

func (f *rotationAccountForge) Owner(context.Context, forge.ThreadRef) (string, error) {
	return "", nil
}

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
		open: func(_ *url.URL, token string) (forge.Forge, error) {
			return &rotationAccountForge{id: token, limited: limited[token], synced: &synced}, nil
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

func TestAddForgeRegistersTheTokensAccount(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	t.Setenv("MSGVAULT_FAKEFORGE_TOKEN", "7")
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.Analytics.AutoBuildCache = false
	var synced []string
	fs := &forgeSource{
		name: rotationForge, label: "Fake forge", tokenEnv: "MSGVAULT_FAKEFORGE_TOKEN",
		config: func(*config.Config) config.ForgeConfig { return config.ForgeConfig{} },
		open: func(base *url.URL, token string) (forge.Forge, error) {
			assert.Equal("https://forge.example.com/git", base.String())
			return &rotationAccountForge{id: token, synced: &synced}, nil
		},
	}
	cmd := fs.addCommand()
	cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"--url", "https://Forge.example.com/git/"})

	require.NoError(cmd.Execute())
	assert.Contains(output.String(), "Added Fake forge account user7")
	identifier := forge.SourceIdentifier("forge.example.com/git", "7")
	token, err := forge.LoadToken(cfg.TokensDir(), rotationForge, identifier)
	require.NoError(err)
	assert.Equal("7", token.AccessToken)
	assert.Equal("https://forge.example.com/git", token.BaseURL)

	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	defer func() { assert.NoError(st.Close()) }()
	source, err := st.GetSourceByTypeAndIdentifier(t.Context(), rotationForge, identifier)
	require.NoError(err)
	assert.Equal("Fake forge user7 (forge.example.com/git)", source.DisplayName.String)
	identities, err := st.ListAccountIdentities(source.ID)
	require.NoError(err)
	require.Len(identities, 1)
	assert.Equal(forge.AccountKey("forge.example.com/git", "7"), identities[0].Address, "the account's posts count as yours")
}

func TestForgeSyncRemembersARenamedAccountsLogin(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := testConfigValue()
	cfg.Data.DataDir = t.TempDir()
	var synced []string
	fs := &forgeSource{
		name: rotationForge, label: "Fake forge",
		config: func(*config.Config) config.ForgeConfig { return config.ForgeConfig{} },
		open: func(_ *url.URL, token string) (forge.Forge, error) {
			return &rotationAccountForge{id: token, synced: &synced}, nil
		},
	}
	identifier := forge.SourceIdentifier("forge.example.com", "7")
	source, err := st.GetOrCreateSourceContext(t.Context(), rotationForge, identifier)
	require.NoError(err)
	require.NoError(st.UpdateSourceDisplayNameContext(t.Context(), source.ID, "Fake forge old-name (forge.example.com)"))
	source, err = st.GetSourceByTypeAndIdentifier(t.Context(), rotationForge, identifier)
	require.NoError(err)
	// The account was added as old-name and renamed to user7 since.
	require.NoError(forge.SaveToken(cfg.TokensDir(), rotationForge, forge.Token{
		AccessToken: "7", BaseURL: "https://forge.example.com", Instance: "forge.example.com", AccountID: "7", Login: "old-name",
	}))

	_, err = fs.sync(t.Context(), st, cfg, source, false)
	require.NoError(err)

	found, err := fs.sources(t.Context(), st, cfg.TokensDir(), "user7")
	require.NoError(err, "the account is found by its current login")
	require.Len(found, 1)
	assert.Equal("Fake forge user7 (forge.example.com)", found[0].DisplayName.String, "the source carries its current login")
}
