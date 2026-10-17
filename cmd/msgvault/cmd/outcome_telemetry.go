package cmd

import (
	"context"
	"strings"

	"go.kenn.io/msgvault/internal/api"
)

func (a *storeAPIAdapter) recordSyncOutcome(req api.CLISyncRequest, err error) {
	if a.outcomes == nil {
		return
	}
	key := ""
	if req.Email != "" {
		key = "account:" + strings.ToLower(strings.TrimSpace(req.Email))
		if a.store != nil && !req.SourceIDSet {
			sources, lookupErr := a.store.GetSourcesByIdentifierOrDisplayName(req.Email)
			if lookupErr == nil && len(sources) == 1 {
				req.SourceID, req.SourceIDSet = sources[0].ID, true
			}
		}
	}
	if req.SourceIDSet && a.store != nil {
		source, lookupErr := a.store.GetSourceByID(req.SourceID)
		if lookupErr != nil {
			return
		}
		if job, ok := api.SchedulerJobNameForSource(source.SourceType, source.Identifier); ok {
			key = job
		} else {
			key = "account:" + strings.ToLower(strings.TrimSpace(source.Identifier))
		}
	}
	if reportErr := a.outcomes.SyncResult(key, err); reportErr != nil && a.logger != nil {
		a.logger.Warn("record sync outcome", "error", reportErr)
	}
}

func (a *storeAPIAdapter) recordCLICommandSyncOutcome(ctx context.Context, args []string, paused bool, err error) {
	if !manualSyncCLICommand(args) || paused || ctx.Err() != nil {
		return
	}
	key := ""
	kind := strings.TrimPrefix(args[0], "sync-")
	selector := ""
	command, _, _ := rootCmd.Find(args[:1])
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if i+1 < len(args) {
				selector = args[i+1]
			}
			break
		}
		if !strings.HasPrefix(arg, "-") {
			selector = arg
			break
		}
		name, _, assigned := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if assigned {
			continue
		}
		flag := command.Flags().Lookup(name)
		if flag == nil {
			flag = rootCmd.PersistentFlags().Lookup(name)
		}
		if flag != nil && flag.NoOptDefVal == "" {
			i++
		}
	}
	switch {
	case kind == "slack" || kind == "beeper" || kind == "matrix":
		key = kind
	case selector != "":
		switch kind {
		case "calendar":
			email := selector
			if a.config != nil {
				if source := a.config.GetGCalSource(selector); source != nil {
					email = source.Email
				}
			}
			key = api.GCalJobNameForAccountEmail(strings.ToLower(strings.TrimSpace(email)))
		case "teams", "discord":
			key = "account:" + strings.ToLower(strings.TrimSpace(selector))
		case "chatwoot":
			if a.config != nil {
				if source := a.config.GetChatwootSource(selector); source != nil {
					key, _ = api.ChatwootJobNameForAccount(source.URL, source.AccountID)
				}
			}
		case "synctech-sms":
			if a.config != nil {
				if source := a.config.GetSynctechSMSSource(selector); source != nil {
					selector = source.OwnerPhone
				}
			}
			key, _ = api.SchedulerJobNameForSource(strings.ReplaceAll(kind, "-", "_"), selector)
		default:
			key, _ = api.SchedulerJobNameForSource(strings.ReplaceAll(kind, "-", "_"), selector)
		}
	}
	if reportErr := a.outcomes.SyncResult(key, err); reportErr != nil && a.logger != nil {
		a.logger.Warn("record sync outcome", "error", reportErr)
	}
}
