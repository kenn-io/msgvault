package api

import (
	"encoding/json/jsontext"
	"errors"
	"io"
	"strings"

	"github.com/spf13/pflag"
	"go.kenn.io/msgvault/internal/opserr"
)

// BroadcastSubcommands excludes opt-in requests until the DM send adapter exists.
var BroadcastSubcommands = []string{"subscriptions", "sync-subscriptions", "create", "plan", "show", "list", "preview", "send", "adopt-campaign", "cancel", "unsubscribe"}

type BroadcastIntent struct {
	Sub, Profile, ID                string
	Revision                        int64
	PlanDigest, Name                string
	MessageJSON                     jsontext.Value
	DailyBudgetCents, MinGapSeconds int64
	Participants, Subscriptions     []int64
	Limit                           int
	CampaignID, Status              string
	MaxRequests                     int
	JSON                            bool
	message                         string
}

// BindBroadcastFlags keeps Cobra and the daemon admission parser on one grammar.
func BindBroadcastFlags(fs *pflag.FlagSet, sub string, in *BroadcastIntent) error {
	fs.BoolVar(&in.JSON, "json", false, "emit a JSON result")
	switch sub {
	case "subscriptions":
		fs.Int64SliceVar(&in.Participants, "participant", nil, "participant ID, repeatable")
		fs.StringVar(&in.Status, "status", "", "subscription status")
	case "sync-subscriptions":
		fs.IntVar(&in.MaxRequests, "max-requests", 1000, "maximum token-list requests")
	case "create":
		fs.StringVar(&in.Name, "name", "", "broadcast name")
		fs.StringVar(&in.message, "message-json", "", "message object, up to 32 KiB")
		fs.Int64Var(&in.DailyBudgetCents, "daily-budget-cents", 0, "average daily campaign budget in cents")
		fs.Int64Var(&in.MinGapSeconds, "min-conversation-gap-seconds", 0, "minimum conversation gap in seconds")
		fs.Int64SliceVar(&in.Participants, "participant", nil, "participant ID, repeatable")
		fs.Int64SliceVar(&in.Subscriptions, "subscription", nil, "subscription ID, repeatable")
	case "plan":
		fs.Int64Var(&in.Revision, "revision", 0, "broadcast revision to plan")
	case "send":
		fs.Int64Var(&in.Revision, "revision", 0, "confirmed broadcast revision")
		fs.StringVar(&in.PlanDigest, "plan-digest", "", "confirmed plan digest")
		fs.IntVar(&in.Limit, "limit", 100, "maximum dispatches in this run")
	case "adopt-campaign":
		fs.StringVar(&in.CampaignID, "campaign-id", "", "existing campaign ID")
	case "unsubscribe":
		fs.Int64SliceVar(&in.Participants, "participant", nil, "participant to suppress")
		fs.Int64SliceVar(&in.Subscriptions, "subscription", nil, "subscription to suppress")
	case "show", "list", "preview", "cancel":
	default:
		return errors.New("unknown broadcast command")
	}
	return nil
}

func ParseCLIRunBroadcast(req CLIRunRequest) (BroadcastIntent, error) {
	var in BroadcastIntent
	invalid := func() (BroadcastIntent, error) { return BroadcastIntent{}, errors.New("invalid broadcast arguments") }
	invalidValue := func() (BroadcastIntent, error) {
		return BroadcastIntent{}, opserr.Invalid(errors.New("invalid broadcast value"))
	}
	if len(req.Args) < 3 || req.Args[0] != "broadcast" || len(req.Env) > 0 || req.Cwd != "" || req.GrantDecided {
		return invalid()
	}
	in.Sub = req.Args[1]
	fs := pflag.NewFlagSet("broadcast", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := BindBroadcastFlags(fs, in.Sub, &in); err != nil {
		return invalid()
	}
	if err := fs.Parse(req.Args[2:]); err != nil {
		if strings.HasPrefix(err.Error(), "invalid argument ") || strings.HasPrefix(err.Error(), "invalid boolean value ") {
			return BroadcastIntent{}, opserr.Invalid(err)
		}
		return invalid()
	}
	if fs.NArg() != 1 || strings.TrimSpace(fs.Arg(0)) == "" {
		return invalid()
	}
	switch in.Sub {
	case "subscriptions", "sync-subscriptions", "create", "list", "unsubscribe":
		in.Profile = fs.Arg(0)
	default:
		in.ID = fs.Arg(0)
	}
	if len(in.Participants)+len(in.Subscriptions) > 1000 {
		return invalidValue()
	}
	for _, ids := range [][]int64{in.Participants, in.Subscriptions} {
		for _, id := range ids {
			if id <= 0 {
				return invalidValue()
			}
		}
	}
	switch in.Sub {
	case "create":
		if in.Name == "" || !fs.Changed("message-json") || !fs.Changed("daily-budget-cents") || (len(in.Participants) == 0) == (len(in.Subscriptions) == 0) {
			return invalid()
		}
		in.MessageJSON = jsontext.Value(in.message)
		if in.DailyBudgetCents <= 0 || in.MinGapSeconds < 0 || len(in.MessageJSON) > 32<<10 || !in.MessageJSON.IsValid() || in.MessageJSON.Kind() != '{' {
			return invalidValue()
		}
	case "plan":
		if !fs.Changed("revision") {
			return invalid()
		}
		if in.Revision <= 0 {
			return invalidValue()
		}
	case "send":
		if !fs.Changed("revision") || in.PlanDigest == "" {
			return invalid()
		}
		if in.Revision <= 0 || in.Limit < 1 || in.Limit > 1000 {
			return invalidValue()
		}
	case "adopt-campaign":
		if in.CampaignID == "" {
			return invalid()
		}
		if strings.IndexFunc(in.CampaignID, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return invalidValue()
		}
	case "unsubscribe":
		if len(in.Participants)+len(in.Subscriptions) != 1 {
			return invalid()
		}
	case "sync-subscriptions":
		if in.MaxRequests < 1 {
			return invalidValue()
		}
	case "subscriptions":
		switch in.Status {
		case "", "active", "stopped", "unsubscribed", "absent":
		default:
			return invalidValue()
		}
	}
	return in, nil
}
