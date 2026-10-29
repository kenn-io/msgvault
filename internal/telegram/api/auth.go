package api

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// ErrLoginInputRequired is a safe local prompt error that can be shown to users.
var ErrLoginInputRequired = errors.New("Telegram login input is required")

// ErrLoginInputRead reports that local prompt input could not be read.
var ErrLoginInputRead = errors.New("cannot read Telegram login input")

// ErrSignupUnsupported reports that this client does not create Telegram accounts.
var ErrSignupUnsupported = errors.New("creating Telegram accounts is not supported")

// LoginPhone performs the phone, code and optional two-factor flow.
func LoginPhone(ctx context.Context, client *tg.Client, appID int, appHash string, prompts auth.UserAuthenticator) error {
	authClient := auth.NewClient(client, rand.Reader, appID, appHash)
	if err := auth.NewFlow(prompts, auth.SendCodeOptions{}).Run(ctx, authClient); err != nil {
		return safeAuthenticationError(ctx, err)
	}
	return nil
}

// LoginQR displays a login token and waits for the Telegram confirmation.
func LoginQR(ctx context.Context, client *tg.Client, appID int, appHash string, loggedIn <-chan struct{}, show func(context.Context, qrlogin.Token) error, options qrlogin.Options) (*tg.AuthAuthorization, error) {
	return LoginQRWithPassword(ctx, client, appID, appHash, loggedIn, show, options, nil)
}

// LoginQRWithPassword supports Telegram accounts with two-factor authentication.
func LoginQRWithPassword(ctx context.Context, client *tg.Client, appID int, appHash string, loggedIn <-chan struct{}, show func(context.Context, qrlogin.Token) error, options qrlogin.Options, prompts auth.UserAuthenticator) (*tg.AuthAuthorization, error) {
	qr := qrlogin.NewQR(client, appID, appHash, options)
	result, err := qr.Auth(ctx, loggedIn, show)
	var migration *qrlogin.MigrationNeededError
	if errors.As(err, &migration) && options.Migrate != nil && migration.MigrateTo != nil {
		err = options.Migrate(ctx, migration.MigrateTo.DCID)
		if err == nil {
			var imported tg.AuthLoginTokenClass
			imported, err = client.AuthImportLoginToken(ctx, migration.MigrateTo.Token)
			if err == nil {
				switch token := imported.(type) {
				case *tg.AuthLoginToken:
					// The imported token is pending. Export a fresh one on the
					// migrated connection and resume the normal display flow.
					result, err = qr.Auth(ctx, loggedIn, show)
				case *tg.AuthLoginTokenSuccess:
					var ok bool
					result, ok = token.Authorization.(*tg.AuthAuthorization)
					if !ok {
						err = errors.New("unexpected Telegram authorization")
					}
				default:
					err = errors.New("unexpected Telegram login token response")
				}
			}
		}
	}
	if tgerr.Is(err, "SESSION_PASSWORD_NEEDED") && prompts != nil {
		clientAuth := auth.NewClient(client, rand.Reader, appID, appHash)
		if hashed, ok := prompts.(auth.PasswordHashProvider); ok {
			result, err = clientAuth.PasswordWith(ctx, hashed.PasswordHash)
		} else {
			password, promptErr := prompts.Password(ctx)
			if promptErr != nil {
				return nil, safeAuthenticationError(ctx, promptErr)
			}
			result, err = clientAuth.Password(ctx, password)
		}
	}
	if err != nil {
		return nil, safeAuthenticationError(ctx, err)
	}
	return result, nil
}

// Owner resolves the authenticated account identity from Telegram.
func Owner(ctx context.Context, client *tg.Client) (*tg.User, error) {
	full, err := client.UsersGetFullUser(ctx, &tg.InputUserSelf{})
	if err != nil {
		return nil, fmt.Errorf("get Telegram account identity: %w", err)
	}
	for _, entry := range full.Users {
		if user, ok := entry.(*tg.User); ok && user.Self && user.ID > 0 && user.ID == full.FullUser.ID {
			return user, nil
		}
	}
	return nil, fmt.Errorf("Telegram account identity is missing or inconsistent")
}

// PersistAuthenticatedSession resolves the owner before naming the credential file.
func PersistAuthenticatedSession(ctx context.Context, client *tg.Client, temporary session.Storage, dir string, appID int, appHash string) (*tg.User, error) {
	owner, err := Owner(ctx, client)
	if err != nil {
		return nil, err
	}
	data, err := temporary.LoadSession(ctx)
	if err != nil {
		return nil, fmt.Errorf("load authenticated Telegram session: %w", err)
	}
	store := NewSessionStore(dir, appID, appHash, owner.ID)
	if err := store.StoreSession(ctx, data); err != nil {
		return nil, err
	}
	return owner, nil
}

func safeAuthenticationError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, ErrLoginInputRequired) {
		return ErrLoginInputRequired
	}
	if errors.Is(err, ErrLoginInputRead) {
		return ErrLoginInputRead
	}
	if errors.Is(err, ErrSignupUnsupported) {
		return ErrSignupUnsupported
	}
	var migration *qrlogin.MigrationNeededError
	if errors.As(err, &migration) {
		return errors.New("Telegram login requires a data center migration")
	}
	if wait, ok := tgerr.AsFloodWait(err); ok && wait >= 0 {
		return fmt.Errorf("Telegram rate limit requires a wait of %s", wait)
	}
	if errors.Is(err, auth.ErrPasswordInvalid) {
		return errors.New("Telegram two-factor password is invalid")
	}
	switch {
	case tgerr.Is(err, "PHONE_CODE_INVALID"):
		return errors.New("Telegram verification code is invalid")
	case tgerr.Is(err, "PHONE_CODE_EXPIRED"):
		return errors.New("Telegram verification code has expired")
	case tgerr.Is(err, "SESSION_PASSWORD_NEEDED"):
		return errors.New("Telegram two-factor password is required")
	default:
		return errors.New("Telegram authentication failed")
	}
}
