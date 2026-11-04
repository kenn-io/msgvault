package metawebhook

import (
	"context"
	"strings"
)

type Class string

const (
	Applied   Class = "applied"
	Ignored   Class = "ignored"
	Unhandled Class = "unhandled"
	Unrouted  Class = "unrouted"
	Retry     Class = "retry"
	Invalid   Class = "invalid"
)

type ItemOutcome struct {
	Class        Class
	ErrorClass   string
	CountAttempt bool
	// ArchiveChanged includes committed partial writes even when the item retries.
	ArchiveChanged bool
}
type ItemHandler func(context.Context, string, []Item) []ItemOutcome

type Registry struct{ handlers map[string]ItemHandler }

func NewRegistry() *Registry { return &Registry{handlers: map[string]ItemHandler{}} }

var fieldAllowlist = map[string]string{
	"page":      " feed mention messages message_echoes message_reactions message_edits message_reads message_deliveries messaging_postbacks messaging_referrals messaging_optins messaging_handovers standby messaging_unknown ",
	"instagram": " comments live_comments mentions messages message_reactions messaging_postbacks messaging_referral messaging_seen messaging_unknown ",
}

// Register is a startup operation. A duplicate or unsupported pair is a bug.
func (r *Registry) Register(object, field string, h ItemHandler) {
	key := object + "/" + field
	if field == "" || strings.ContainsAny(field, " /\t\n") || !strings.Contains(fieldAllowlist[object], " "+field+" ") || h == nil || r.handlers[key] != nil {
		panic("invalid or duplicate Meta webhook handler")
	}
	r.handlers[key] = h
}
func (r *Registry) Lookup(object, field string) (ItemHandler, bool) {
	h, ok := r.handlers[object+"/"+field]
	return h, ok
}

// Settle retains the entire delivery if any item remains unapplied.
func Settle(outcomes []ItemOutcome, items []Item) Settlement {
	result := Settlement{State: StateDone, Outcomes: map[string]int{}}
	for index, item := range items {
		out := ItemOutcome{Class: Invalid, ErrorClass: "handler_outcome_missing"}
		if index < len(outcomes) {
			out = outcomes[index]
		}
		result.Outcomes[item.Object+"/"+item.Field+"/"+string(out.Class)]++
		switch out.Class {
		case Retry:
			result.State = StatePending
			result.CountAttempt = result.CountAttempt || out.CountAttempt
			if out.ErrorClass != "" && (out.CountAttempt || !result.CountAttempt) {
				result.ErrorClass = out.ErrorClass
			}
		case Applied, Ignored:
		default:
			if result.State != StatePending {
				result.State = StateHeld
			}
		}
	}
	return result
}
