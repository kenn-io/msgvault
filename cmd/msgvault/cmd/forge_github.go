package cmd

import (
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
		tokenPrompt: "GitHub personal access token (classic, with the notifications scope):",
		addHelp: `Add a GitHub account as an archive source.

Archives the issues, pull requests and discussions the account takes part in,
with every comment and review, and the releases of repositories it watches.
Syncs find changed threads through the account's notifications and a search
for threads that involve it, so threads it only commented on are archived too.

Requires a classic personal access token: GitHub's notifications API doesn't
accept fine-grained tokens. Create one at https://github.com/settings/tokens
with the notifications scope; add repo to archive private repositories, and
read:user to link GitHub accounts to the people in your archive by their
public email.

Provide the token via --token-file, the MSGVAULT_GITHUB_TOKEN environment
variable, or the interactive prompt. Choose whose repositories are archived
with include_owners and exclude_owners under [github] in config.toml.

Examples:
  msgvault add-github
  msgvault add-github --token-file ~/github-token.txt
  MSGVAULT_GITHUB_TOKEN="ghp_..." msgvault add-github`,
		config: func(c *config.Config) config.ForgeConfig { return c.GitHub },
		open: func(_ *url.URL, token string) forge.Forge {
			api, err := url.Parse(github.APIURL)
			if err != nil {
				panic(err) // a constant URL always parses
			}
			return github.New(api, token, forgeUserAgent())
		},
	})
}
