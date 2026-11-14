package cmd

import (
	"net/url"

	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/forge"
	"go.kenn.io/msgvault/internal/forge/gitea"
)

func init() {
	registerForge(&forgeSource{ // #nosec G101 -- a token prompt and env var name, not a credential.
		name:        gitea.Name,
		label:       "Gitea",
		tokenEnv:    clirun.EnvGiteaToken,
		tokenPrompt: "Gitea or Forgejo access token (with the read:notification, read:issue, read:repository and read:user scopes):",
		addHelp: `Add a Gitea or Forgejo account, such as one on Codeberg, as an archive
source.

Archives the issues and pull requests the account takes part in, with every
comment, review and review comment. Includes threads the account was
notified about, authored, was assigned, mentioned in, asked to review or
commented on, and changes to threads already archived.

Requires --url, the server's web address, and an access token with the
read:notification, read:issue, read:repository and read:user scopes.

Provide the token via --token-file, the MSGVAULT_GITEA_TOKEN environment
variable, or the interactive prompt. Choose whose repositories are archived
with include_owners and exclude_owners under [gitea] in config.toml.

Examples:
  msgvault add-gitea --url https://codeberg.org
  msgvault add-gitea --url https://git.example.com --token-file ~/gitea-token.txt
  MSGVAULT_GITEA_TOKEN="..." msgvault add-gitea --url https://codeberg.org`,
		config: func(c *config.Config) config.ForgeConfig { return c.Gitea },
		open: func(base *url.URL, token string) (forge.Forge, error) {
			return gitea.New(base, token, forgeUserAgent())
		},
	})
}
