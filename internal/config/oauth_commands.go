package config

import (
	"errors"
	"fmt"
	"strings"
)

// OAuthTokenCommands replace Google token files with an external store.
// Listing is required so Gmail alias checks also cover unregistered accounts.
type OAuthTokenCommands struct {
	ReadCommand   []string `toml:"read_command"`
	WriteCommand  []string `toml:"write_command"`
	DeleteCommand []string `toml:"delete_command"`
	ListCommand   []string `toml:"list_command"`
}

func (c OAuthTokenCommands) Enabled() bool {
	return c.ReadCommand != nil || c.WriteCommand != nil || c.DeleteCommand != nil || c.ListCommand != nil
}

func validateSecretCommand(argv []string) error {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return errors.New("command must have a nonblank executable")
	}
	for _, arg := range argv {
		if strings.ContainsRune(arg, 0) {
			return errors.New("command arguments cannot contain NUL")
		}
	}
	return nil
}

func (c OAuthTokenCommands) Validate() error {
	if !c.Enabled() {
		return nil
	}
	for _, field := range []struct {
		name string
		argv []string
	}{
		{"read_command", c.ReadCommand}, {"write_command", c.WriteCommand}, {"delete_command", c.DeleteCommand}, {"list_command", c.ListCommand},
	} {
		if len(field.argv) == 0 {
			return fmt.Errorf("tokens.%s: command is required when any token command is configured", field.name)
		}
		if err := validateSecretCommand(field.argv); err != nil {
			return fmt.Errorf("tokens.%s: %w", field.name, err)
		}
	}
	return nil
}

func validateOAuthApp(app OAuthApp) error {
	if app.ClientSecretsCommand == nil {
		return nil
	}
	if app.ClientSecrets != "" {
		return errors.New("client_secrets and client_secrets_command are mutually exclusive")
	}
	if err := validateSecretCommand(app.ClientSecretsCommand); err != nil {
		return fmt.Errorf("client_secrets_command: %w", err)
	}
	return nil
}

func (o *OAuthConfig) Validate() error {
	if err := validateOAuthApp(OAuthApp{ClientSecrets: o.ClientSecrets, ClientSecretsCommand: o.ClientSecretsCommand}); err != nil {
		return err
	}
	for name, app := range o.Apps {
		if err := validateOAuthApp(app); err != nil {
			return fmt.Errorf("apps.%s: %w", name, err)
		}
	}
	return o.Tokens.Validate()
}

// CredentialsFor resolves a Google client source without executing its command.
// Named apps never inherit the default credentials.
func (o *OAuthConfig) CredentialsFor(name string) (OAuthApp, error) {
	app := OAuthApp{ClientSecrets: o.ClientSecrets, ClientSecretsCommand: o.ClientSecretsCommand, ServiceAccountKey: o.ServiceAccountKey}
	if name != "" {
		var ok bool
		app, ok = o.Apps[name]
		if !ok {
			return OAuthApp{}, fmt.Errorf("OAuth app %q not configured", name)
		}
	}
	if err := validateOAuthApp(app); err != nil {
		return OAuthApp{}, err
	}
	if app.ClientSecrets == "" && app.ClientSecretsCommand == nil {
		_, err := o.ClientSecretsFor(name)
		return OAuthApp{}, err
	}
	app.ClientSecretsCommand = append([]string(nil), app.ClientSecretsCommand...)
	return app, nil
}
