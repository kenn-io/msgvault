package cmd

import (
	"net/url"

	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/forge"
	"go.kenn.io/msgvault/internal/forge/gitlab"
)

func init() {
	registerForge(&forgeSource{
		name:        gitlab.Name,
		label:       "GitLab",
		defaultURL:  gitlab.DefaultURL,
		tokenEnv:    clirun.EnvGitLabToken,
		tokenPrompt: "GitLab personal access token (with the read_api and read_user scopes):",
		addHelp: `Add a GitLab account as an archive source.

Archives the issues and merge requests the account takes part in, with every
comment, thread reply, diff note and approval. Includes threads in the
account's to-dos or activity, and those it authored, is assigned or reviews.
GitLab keeps activity three years, discussion replies included. Archived
threads are checked for later replies and edits.

Requires a personal access token with the read_api and read_user scopes.
Without --url the account is on gitlab.com; name a self-managed instance by
its web address.

Provide the token via --token-file, the MSGVAULT_GITLAB_TOKEN environment
variable, or the interactive prompt. Choose whose projects are archived with
include_owners and exclude_owners under [gitlab] in config.toml: an owner is
a project's top-level group or user.

Examples:
  msgvault add-gitlab
  msgvault add-gitlab --url https://gitlab.example.com
  MSGVAULT_GITLAB_TOKEN="glpat-..." msgvault add-gitlab --token-file ~/gitlab-token.txt`,
		config: func(c *config.Config) config.ForgeConfig { return c.GitLab },
		open: func(base *url.URL, token string) (forge.Forge, error) {
			return gitlab.New(base.String(), token, forgeUserAgent())
		},
	})
}
