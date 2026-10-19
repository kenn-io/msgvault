package daemonclient

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// OpenLoopKataProject reads the daemon target for a write disclosure. Do not
// cache it: a long-lived MCP process can outlive a daemon restart.
func (c *Client) OpenLoopKataProject(ctx context.Context) (string, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.GetSettingsResp, error) {
		return api.GetSettingsWithResponse(ctx)
	})
	if err != nil {
		return "", fmt.Errorf("read daemon Kata project: %w", err)
	}
	if response.JSON200 == nil {
		return "", errors.New("daemon Kata project settings are unavailable")
	}
	if response.JSON200.PendingRestart {
		return "", errors.New("restart the daemon before confirming Kata loop tracking: settings have pending changes")
	}
	for _, setting := range response.JSON200.Settings {
		if setting.Key != "integrations.kata.default_project" || setting.Value == nil || setting.Value.SettingValue_OneOf == nil {
			continue
		}
		value, err := setting.Value.SettingValue_OneOf.AsValidatedSettingValue_OneOf_0()
		if err == nil && strings.TrimSpace(value.String) != "" {
			return strings.TrimSpace(value.String), nil
		}
	}
	return "", errors.New("daemon Kata project is unavailable in settings")
}
