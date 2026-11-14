package cmd

import (
	"fmt"
	"net/url"

	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/forge"
	"go.kenn.io/msgvault/internal/forge/github"
)

func init() {
	registerForge(&forgeSource{
		name:        github.Name,
		label:       "GitHub",
		defaultURL:  "https://github.com",
		fixedURL:    true,
		tokenEnv:    clirun.EnvGitHubToken,
		tokenPrompt: "GitHub personal access token (classic, with the notifications and public_repo scopes, or repo):",
		addHelp: `Add a GitHub account as an archive source.

Archives the issues, pull requests and discussions the account takes part in,
with every comment and review, and the releases of repositories it watches:
threads it was notified about, and threads it only commented on.

Requires a classic personal access token: GitHub's notifications API doesn't
accept fine-grained tokens. Create one at https://github.com/settings/tokens
with the notifications and public_repo scopes, or repo to archive private
repositories too; add read:user to link GitHub accounts to the people in your
archive by their public email.

Provide the token via --token-file, the MSGVAULT_GITHUB_TOKEN environment
variable, or the interactive prompt. Choose whose repositories are archived
with include_owners and exclude_owners under [github] in config.toml.

Examples:
  msgvault add-github
  msgvault add-github --token-file ~/github-token.txt
  MSGVAULT_GITHUB_TOKEN="ghp_..." msgvault add-github`,
		config: func(c *config.Config) config.ForgeConfig { return c.GitHub },
		open: func(_ *url.URL, token string) (forge.Forge, error) {
			api, err := url.Parse(github.APIURL)
			if err != nil {
				return nil, fmt.Errorf("parse the GitHub API URL: %w", err)
			}
			return github.New(api, token, forgeUserAgent()), nil
		},
	})
}
