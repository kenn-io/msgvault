package api

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/gotd/td/tgmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOwnerIdentity(t *testing.T) {
	mock := tgmock.NewRequire(t)
	mock.ExpectFunc(func(input bin.Encoder) {
		req, ok := input.(*tg.UsersGetFullUserRequest)
		require.True(t, ok)
		assert.IsType(t, &tg.InputUserSelf{}, req.ID)
	}).ThenResult(&tg.UsersUserFull{FullUser: tg.UserFull{ID: 456}, Users: []tg.UserClass{&tg.User{ID: 456, Self: true}}})
	owner, err := Owner(context.Background(), tg.NewClient(mock))
	require.NoError(t, err)
	assert.Equal(t, int64(456), owner.ID)
	assert.True(t, mock.AllWereMet())
}

type passwordHashPrompts struct{ auth.UserAuthenticator }

func (p passwordHashPrompts) PasswordHash(context.Context, *tg.AccountPassword) (*tg.InputCheckPasswordSRP, error) {
	return &tg.InputCheckPasswordSRP{SRPID: 123, A: []byte("synthetic-a"), M1: []byte("synthetic-m1")}, nil
}

func TestLoginErrorsDoNotExposeCredentials(t *testing.T) {
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenErr(errors.New("synthetic-secret"))
	prompts := auth.Constant("+15550100100", "", auth.CodeAuthenticatorFunc(func(context.Context, *tg.AuthSentCode) (string, error) { return "12345", nil }))
	err := LoginPhone(context.Background(), tg.NewClient(mock), 123, "synthetic-secret", prompts)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "synthetic-secret")
}

func TestLoginQRTwoFactor(t *testing.T) {
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.AuthLoginToken{Expires: int(time.Now().Add(time.Minute).Unix()), Token: []byte("synthetic-token")})
	mock.Expect().ThenRPCErr(tgerr.New(401, "SESSION_PASSWORD_NEEDED"))
	password := &tg.AccountPassword{NewAlgo: &tg.PasswordKdfAlgoUnknown{}, NewSecureAlgo: &tg.SecurePasswordKdfAlgoUnknown{}}
	password.SetSRPID(123)
	password.SetCurrentAlgo(&tg.PasswordKdfAlgoUnknown{})
	mock.Expect().ThenResult(password)
	mock.Expect().ThenResult(&tg.AuthAuthorization{User: &tg.User{ID: 456}})
	loggedIn := make(chan struct{})
	close(loggedIn)
	prompts := passwordHashPrompts{auth.Constant("+15550100100", "", auth.CodeAuthenticatorFunc(func(context.Context, *tg.AuthSentCode) (string, error) { return "12345", nil }))}
	result, err := LoginQRWithPassword(context.Background(), tg.NewClient(mock), 123, "synthetic-hash", loggedIn, func(context.Context, qrlogin.Token) error { return nil }, qrlogin.Options{}, prompts)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, mock.AllWereMet())
}

func TestLoginQRReportsRequiredMigration(t *testing.T) {
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.AuthLoginTokenMigrateTo{DCID: 4, Token: []byte("synthetic")})
	_, err := LoginQR(context.Background(), tg.NewClient(mock), 123, "synthetic-hash", make(chan struct{}), func(context.Context, qrlogin.Token) error { return nil }, qrlogin.Options{})
	require.ErrorContains(t, err, "data center")
}

func TestLoginQRMigratesDuringInitialExport(t *testing.T) {
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.AuthLoginTokenMigrateTo{DCID: 4, Token: []byte("synthetic-token")})
	mock.ExpectFunc(func(input bin.Encoder) {
		req, ok := input.(*tg.AuthImportLoginTokenRequest)
		require.True(t, ok)
		assert.Equal(t, []byte("synthetic-token"), req.Token)
	}).ThenResult(&tg.AuthLoginTokenSuccess{Authorization: &tg.AuthAuthorization{User: &tg.User{ID: 456}}})
	migrated := 0
	options := qrlogin.Options{Migrate: func(_ context.Context, dc int) error { migrated = dc; return nil }}
	result, err := LoginQR(context.Background(), tg.NewClient(mock), 123, "synthetic-hash", make(chan struct{}), func(context.Context, qrlogin.Token) error { return nil }, options)
	{
		require := require.New(t)
		require.NoError(err)
	}
	require.NotNil(t, result)
	{
		assert := assert.New(t)
		assert.Equal(4, migrated)
	}
	assert.True(t, mock.AllWereMet())
}

func TestLoginQRMigratesAndContinuesPendingToken(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.AuthLoginTokenMigrateTo{DCID: 4, Token: []byte("synthetic-token")})
	mock.Expect().ThenResult(&tg.AuthLoginToken{Token: []byte("pending"), Expires: int(time.Now().Add(time.Minute).Unix())})
	mock.Expect().ThenResult(&tg.AuthLoginToken{Token: []byte("new-token"), Expires: int(time.Now().Add(time.Minute).Unix())})
	mock.Expect().ThenResult(&tg.AuthLoginTokenSuccess{Authorization: &tg.AuthAuthorization{User: &tg.User{ID: 456}}})
	signal := make(chan struct{}, 1)
	shown := false
	result, err := LoginQR(context.Background(), tg.NewClient(mock), 123, "synthetic-hash", signal, func(context.Context, qrlogin.Token) error { shown = true; signal <- struct{}{}; return nil }, qrlogin.Options{Migrate: func(context.Context, int) error { return nil }})
	require.NoError(err)
	require.NotNil(result)
	assert.True(shown)
	assert.True(mock.AllWereMet())
}

func TestSafeAuthenticationErrorPreservesOnlyKnownPromptErrors(t *testing.T) {
	assert.ErrorIs(t, safeAuthenticationError(context.Background(), fmt.Errorf("prompt: %w", ErrLoginInputRequired)), ErrLoginInputRequired)
	assert.ErrorIs(t, safeAuthenticationError(context.Background(), fmt.Errorf("prompt: %w", ErrSignupUnsupported)), ErrSignupUnsupported)
	err := safeAuthenticationError(context.Background(), errors.New("synthetic-secret"))
	assert.NotContains(t, err.Error(), "synthetic-secret")
}
