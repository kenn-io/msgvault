package cmd

import (
	"errors"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/cligrammar"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
)

const readMessageIDHint = "find message IDs with 'msgvault search <query> --json'; pass the result's id to 'msgvault show <id>'"
const readParticipantIDHint = "find participant IDs with 'msgvault query \"SELECT id, email_address, display_name FROM participants LIMIT 20\"'; durable person IDs come from 'msgvault person list --json'"

func readDaemonResponseError(err error) error {
	return withHint(err, "run 'msgvault daemon status' and 'msgvault version'; update the client and daemon to the same version")
}

func readMessageNotFound(reference string) error {
	return &cliError{Kind: "not_found", Code: "message_not_found", Message: "message not found: " + reference, Hint: readMessageIDHint, ExitCode: 4, Err: store.ErrMessageNotFound}
}

func readResourceIDHint(kind string) string {
	switch {
	case kind == "participant":
		return readParticipantIDHint
	case strings.Contains(kind, "person"):
		return "find person IDs with 'msgvault person list --json'; inspect a profile with 'msgvault person get <person-id> --json'"
	case kind == "organization":
		return "find organization IDs with 'msgvault organization list --json'"
	case kind == "employment":
		return "find employment IDs with 'msgvault employment list --person <person-id> --json'; person IDs come from 'msgvault person list --json'"
	case kind == "relationship":
		return "find relationship IDs with 'msgvault person relationship list <person-id> --json'"
	case kind == "relationship type":
		return "find relationship type IDs and slugs with 'msgvault relationship-type list --json'"
	case kind == "attribute definition":
		return "find definition IDs and slugs with 'msgvault attribute-definition list --json'"
	case kind == "merge":
		return "find merge IDs with 'msgvault person merge-history <person-id> --json'"
	case kind == "candidate":
		return "find merge review candidates with 'msgvault person merge-show <merge-id> --json'; merge IDs come from 'msgvault person merge-history <person-id> --json'"
	default:
		return "inspect the command's --help for its identifier source and an example"
	}
}

// Use structured API status and codes to select recovery; never classify
// server/user error prose or change the API error's historical text.
func readAPIRecoveryHint(cmd *cobra.Command, err error) string {
	apiErr, ok := errors.AsType[*daemonclient.APIError](err)
	if !ok || cmd == nil {
		return ""
	}
	path := strings.TrimPrefix(strings.Join(cligrammar.Canonical(strings.Fields(cmd.CommandPath())), " "), "msgvault ")
	if strings.HasPrefix(path, "person scoring") {
		switch apiErr.Code {
		case "consent_required", "disclosure_changed", "invalid_disclosure_fingerprint":
			return "review 'msgvault person scoring status --json', then pass its disclosure fingerprint to 'msgvault person scoring consent <disclosure-fingerprint>'"
		case "scoring_disabled", "invalid_config", "credential_unavailable":
			return "inspect readiness and disclosure with 'msgvault person scoring status --json'; ask the archive owner to configure the reported blocker"
		}
	}
	switch apiErr.Code {
	case "person_brief_not_tracked":
		return "inspect the person, then run 'msgvault person track <person-id>' before 'msgvault person brief enroll <person-id>'"
	case "person_brief_not_enrolled", "person_brief_not_found":
		return "inspect 'msgvault person brief history <person-id> --json'; enroll with 'msgvault person brief enroll <person-id> --track' before generating a brief"
	case "person_brief_policy_refused", "person_brief_no_supported_lane":
		return "review 'msgvault person provider status --json'; the archive owner must approve a provider policy that permits the required brief content"
	case "person_brief_lane_disabled":
		return "ask the archive owner to enable [people.sweep.brief] in config.toml, then inspect 'msgvault person provider status --json'"
	case "person_brief_busy":
		return "inspect 'msgvault person sweep status --json' and retry after the active worker finishes"
	}
	if apiErr.Status != http.StatusNotFound && apiErr.Status != http.StatusConflict {
		return ""
	}
	switch {
	case strings.HasPrefix(path, "identity matches"):
		if apiErr.Status == http.StatusConflict {
			return "reload 'msgvault identity matches show <id> --json', review the current evidence, and retry with its --review-token"
		}
		return "find match IDs with 'msgvault identity matches list --json'"
	case strings.HasPrefix(path, "organization attribute"):
		if apiErr.Status == http.StatusConflict {
			return "reload 'msgvault organization attribute list <id> --json' and retry using the current value ID where --expected-value-id is supported"
		}
		return "find organization IDs with 'msgvault organization list --json' and slugs with 'msgvault attribute-definition list --object-type organization --json'"
	case strings.HasPrefix(path, "organization"):
		if apiErr.Status == http.StatusConflict {
			return "reload 'msgvault organization show <id> --json', review its revision and dependencies, then retry the change"
		}
		return "find organization IDs with 'msgvault organization list --json'"
	case strings.HasPrefix(path, "employment"):
		if apiErr.Status == http.StatusConflict {
			return "reload 'msgvault employment show <id> --json', review the current revision and dates, then retry"
		}
		return readResourceIDHint("employment")
	case strings.HasPrefix(path, "attribute-definition"):
		if apiErr.Status == http.StatusConflict {
			return "reload 'msgvault attribute-definition get <definition-id> --json'; review ownership and the current revision before retrying"
		}
		return readResourceIDHint("attribute definition")
	case strings.HasPrefix(path, "relationship-type"):
		return "inspect 'msgvault relationship-type list --json'; built-in or referenced types cannot be deleted"
	case strings.HasPrefix(path, "carddav conflicts"):
		return "refresh conflict IDs with 'msgvault carddav conflicts list', then inspect 'msgvault carddav conflicts show <conflict-id>'"
	case strings.HasPrefix(path, "carddav books"):
		return "find book IDs and their roles with 'msgvault carddav books'"
	case strings.HasPrefix(path, "carddav"):
		return "list saved connections with 'msgvault carddav connections'; configure one with 'msgvault source carddav add --help'"
	case strings.HasPrefix(path, "collection"):
		return "find collection names with 'msgvault collection list'; find member accounts with 'msgvault account list'"
	case strings.HasPrefix(path, "person attributes") || strings.HasPrefix(path, "person notes"):
		if apiErr.Status == http.StatusConflict {
			hint := "reload 'msgvault person attributes list <person-id> --json' and review the current value before retrying"
			if cmd.Flags().Lookup("expected-value-id") != nil {
				hint += " with the current --expected-value-id"
			}
			return hint
		}
		return "find person IDs with 'msgvault person list --json' and definition slugs with 'msgvault attribute-definition list --object-type person --json'"
	case strings.HasPrefix(path, "person relationship"):
		return "list relationships with 'msgvault person relationship list <person-id> --json' and type slugs with 'msgvault relationship-type list --json'"
	case strings.HasPrefix(path, "person"):
		if apiErr.Status == http.StatusConflict {
			hint := "reload 'msgvault person get <person-id> --json', review the latest revision, and retry"
			if cmd.Flags().Lookup("revision") != nil {
				hint += " with the new --revision"
			}
			if cmd.Flags().Lookup("survivor-revision") != nil {
				hint += " with current --survivor-revision and --absorbed-revision values"
			}
			return hint
		}
		return readResourceIDHint("person")
	case strings.HasPrefix(path, "meetings"):
		if apiErr.Code == "not_found" {
			return "run 'msgvault update' on the daemon host, then check 'msgvault daemon status'"
		}
		return "find meeting message IDs with 'msgvault search message_type:meeting_transcript --json'; inspect 'msgvault meetings --help'"
	case strings.HasPrefix(path, "export attachment"):
		return "copy the full attachments[].content_hash from 'msgvault show <message-id> --json'; use 'msgvault export attachments <message-id>' to export all files"
	case strings.HasPrefix(path, "show") || strings.HasPrefix(path, "thread") || strings.HasPrefix(path, "export"):
		return readMessageIDHint
	default:
		return ""
	}
}
