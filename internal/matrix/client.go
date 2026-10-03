// Package matrix implements the native, read-only Matrix archive source.
package matrix

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

const DeviceDisplayName = "msgvault (read-only)"

func validateHomeserverURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("invalid Matrix homeserver URL %q", raw)
	}
	if u.Scheme == "https" {
		return nil
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if u.Scheme == "http" && (host == "localhost" || net.ParseIP(host).IsLoopback()) {
		return nil
	}
	return fmt.Errorf("matrix homeserver URL must use HTTPS unless its host is loopback: %q", raw)
}

// Runtime owns the mautrix client for an account.
type Runtime struct {
	Client *mautrix.Client
}

// Login creates a dedicated Matrix device using a password or m.login.token.
func Login(ctx context.Context, homeserver, userID, secret string, tokenLogin bool) (Credentials, error) {
	if err := validateHomeserverURL(homeserver); err != nil {
		return Credentials{}, err
	}
	cli, err := mautrix.NewClient(homeserver, id.UserID(userID), "")
	if err != nil {
		return Credentials{}, fmt.Errorf("create Matrix client: %w", err)
	}
	req := &mautrix.ReqLogin{
		Type:                     mautrix.AuthTypePassword,
		Identifier:               mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: userID},
		Password:                 secret,
		InitialDeviceDisplayName: DeviceDisplayName,
		StoreCredentials:         true,
	}
	var resp *mautrix.RespLogin
	if tokenLogin {
		// ReqLogin always serializes Identifier, but m.login.token does not use
		// the password-only identifier fields.
		resp = &mautrix.RespLogin{}
		_, err = cli.MakeFullRequest(ctx, mautrix.FullRequest{
			Method: http.MethodPost,
			URL:    cli.BuildClientURL("v3", "login"),
			RequestJSON: struct {
				Type                     mautrix.AuthType `json:"type"`
				Token                    string           `json:"token"`
				InitialDeviceDisplayName string           `json:"initial_device_display_name"`
			}{Type: mautrix.AuthTypeToken, Token: secret, InitialDeviceDisplayName: DeviceDisplayName},
			ResponseJSON:     resp,
			SensitiveContent: true,
		})
	} else {
		resp, err = cli.Login(ctx, req)
	}
	if err != nil {
		return Credentials{}, fmt.Errorf("matrix login: %w", err)
	}
	if resp.UserID == "" || resp.DeviceID == "" || resp.AccessToken == "" {
		return Credentials{}, errors.New("matrix login response is missing user, device, or access token")
	}
	if resp.UserID != id.UserID(userID) {
		mismatchErr := fmt.Errorf("matrix login returned user %s, expected %s", resp.UserID, userID)
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		cleanupErr := Logout(cleanupCtx, Credentials{
			Homeserver: homeserver, UserID: resp.UserID.String(), DeviceID: resp.DeviceID.String(), AccessToken: resp.AccessToken,
		})
		return Credentials{}, errors.Join(mismatchErr, cleanupErr)
	}
	return Credentials{
		Homeserver: homeserver, UserID: resp.UserID.String(), DeviceID: resp.DeviceID.String(), AccessToken: resp.AccessToken,
	}, nil
}

// Open initializes a Matrix client for an account.
func Open(creds Credentials) (*Runtime, error) {
	if err := validateHomeserverURL(creds.Homeserver); err != nil {
		return nil, err
	}
	cli, err := mautrix.NewClient(creds.Homeserver, id.UserID(creds.UserID), creds.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("create Matrix client: %w", err)
	}
	cli.DeviceID = id.DeviceID(creds.DeviceID)
	return &Runtime{Client: cli}, nil
}

// Logout deletes the dedicated Matrix device represented by creds.
func Logout(ctx context.Context, creds Credentials) error {
	if err := validateHomeserverURL(creds.Homeserver); err != nil {
		return err
	}
	cli, err := mautrix.NewClient(creds.Homeserver, id.UserID(creds.UserID), creds.AccessToken)
	if err != nil {
		return fmt.Errorf("create Matrix logout client: %w", err)
	}
	cli.DeviceID = id.DeviceID(creds.DeviceID)
	if _, err := cli.Logout(ctx); err != nil {
		return fmt.Errorf("logout Matrix device %s: %w", creds.DeviceID, err)
	}
	return nil
}

// CheckLogin confirms that creds still authenticate as their user and device.
func CheckLogin(ctx context.Context, creds Credentials) error {
	rt, err := Open(creds)
	if err != nil {
		return err
	}
	resp, err := rt.Client.Whoami(ctx)
	if err != nil {
		return fmt.Errorf("check Matrix device %s: %w", creds.DeviceID, err)
	}
	if resp.UserID.String() != creds.UserID || resp.DeviceID.String() != creds.DeviceID {
		return fmt.Errorf("matrix token belongs to %s device %s, expected %s device %s", resp.UserID, resp.DeviceID, creds.UserID, creds.DeviceID)
	}
	return nil
}

// IsUnknownToken reports whether the homeserver says the credential has
// already been revoked or expired.
func IsUnknownToken(err error) bool {
	return errors.Is(err, mautrix.MUnknownToken)
}
