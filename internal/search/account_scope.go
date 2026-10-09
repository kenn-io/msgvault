package search

import (
	"errors"
	"fmt"
	"net/mail"
	"slices"
	"strings"
)

// AccountScope is one account selection made by a picker or a structured API
// client. Addresses are alternatives, Unattributed adds the rows no confirmed
// account claims, and separate scopes intersect. Inbound limits the scope to
// received mail the way received: does; otherwise sent copies and calendar
// events count too. Scopes travel beside a query, never inside its text.
type AccountScope struct {
	SourceID     *int64   `json:"source_id,omitempty"`
	Addresses    []string `json:"addresses,omitempty"`
	Unattributed bool     `json:"unattributed,omitzero"`
	Inbound      bool     `json:"inbound,omitzero"`
}

const (
	maxAccountScopes         = 16
	maxAccountScopeAddresses = 64
)

// CloneAccountScopes deep-copies scopes so callers can append without
// sharing backing arrays.
func CloneAccountScopes(scopes []AccountScope) []AccountScope {
	if scopes == nil {
		return nil
	}
	out := make([]AccountScope, len(scopes))
	for i, s := range scopes {
		out[i] = s
		if s.SourceID != nil {
			id := *s.SourceID
			out[i].SourceID = &id
		}
		out[i].Addresses = slices.Clone(s.Addresses)
	}
	return out
}

// ParseAccountAddress accepts one exact mailbox and returns it lowercased.
func ParseAccountAddress(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Address != value || !strings.Contains(value, "@") || strings.HasPrefix(value, "@") {
		return "", errors.New("expected an exact email address")
	}
	return value, nil
}

// ValidateAccountScopes checks structured scopes before any SQL is built.
func ValidateAccountScopes(scopes []AccountScope) error {
	if len(scopes) > maxAccountScopes {
		return fmt.Errorf("too many account scopes (maximum %d)", maxAccountScopes)
	}
	for _, s := range scopes {
		if s.SourceID != nil && *s.SourceID <= 0 {
			return errors.New("invalid account scope source ID")
		}
		if len(s.Addresses) > maxAccountScopeAddresses {
			return fmt.Errorf("too many account addresses in one scope (maximum %d)", maxAccountScopeAddresses)
		}
		if len(s.Addresses) == 0 && !s.Unattributed {
			return errors.New("empty account scope")
		}
		for _, a := range s.Addresses {
			if normalized, err := ParseAccountAddress(a); err != nil || normalized != a {
				return fmt.Errorf("invalid account address %q", a)
			}
		}
	}
	return nil
}

// AccountScopesFromQuery expresses the account: and received: operators as
// scopes, for consumers that take scopes rather than a parsed query.
func AccountScopesFromQuery(q *Query) []AccountScope {
	if q == nil {
		return nil
	}
	var scopes []AccountScope
	if len(q.AccountAddrs) > 0 {
		scopes = append(scopes, AccountScope{Addresses: slices.Clone(q.AccountAddrs)})
	}
	if len(q.ReceivedAddrs) > 0 {
		scopes = append(scopes, AccountScope{Addresses: slices.Clone(q.ReceivedAddrs), Inbound: true})
	}
	return scopes
}

// AccountScopeConditions returns one SQL predicate per scope against the
// messages table aliased as alias. Values always bind as parameters.
func AccountScopeConditions(scopes []AccountScope, alias string) ([]string, []any) {
	var conditions []string
	var args []any
	for _, s := range scopes {
		var parts []string
		if s.SourceID != nil {
			parts = append(parts, alias+".source_id = ?")
			args = append(args, *s.SourceID)
		}
		var alternatives []string
		if len(s.Addresses) > 0 {
			for _, a := range s.Addresses {
				args = append(args, a)
			}
			in := alias + ".account_address IN (" + strings.TrimSuffix(strings.Repeat("?,", len(s.Addresses)), ",") + ")"
			if s.Inbound {
				in = "(" + alias + ".account_path = 'inbound' AND " + in + ")"
			}
			alternatives = append(alternatives, in)
		}
		if s.Unattributed {
			if s.Inbound {
				alternatives = append(alternatives,
					"("+alias+".account_path = 'inbound' AND "+alias+".account_address IS NULL)")
			} else {
				// Pending rows (account_path IS NULL) count as unattributed
				// until the repair pass derives them.
				alternatives = append(alternatives,
					"("+alias+".account_address IS NULL AND COALESCE("+alias+".message_type, '') IN ('', 'email', 'calendar_event'))")
			}
		}
		if len(alternatives) == 0 {
			alternatives = append(alternatives, "1 = 0")
		}
		parts = append(parts, "("+strings.Join(alternatives, " OR ")+")")
		conditions = append(conditions, "("+strings.Join(parts, " AND ")+")")
	}
	return conditions, args
}

// AccountScopeConditionsBound renders AccountScopeConditions for callers
// that bind values themselves: bind returns the placeholder for one value.
func AccountScopeConditionsBound(scopes []AccountScope, alias string, bind func(any) string) []string {
	conditions, args := AccountScopeConditions(scopes, alias)
	next := 0
	for i, condition := range conditions {
		// Scope SQL has no "?" outside placeholders.
		parts := strings.Split(condition, "?")
		var b strings.Builder
		for j, part := range parts {
			b.WriteString(part)
			if j < len(parts)-1 {
				b.WriteString(bind(args[next]))
				next++
			}
		}
		conditions[i] = b.String()
	}
	return conditions
}
