package search

import (
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
)

// AccountScope is an OR group of attributed accounts. Separate scopes intersect.
// Inbound restricts received: to inbound mail; account: also includes sent mail
// and calendar events. Groups never expand into thousands of SQL parameters.
type AccountScope struct {
	SourceID *int64 `json:"source_id,omitempty"`

	Addresses    []string `json:"addresses,omitempty"`
	Groups       []string `json:"groups,omitempty"`
	Unattributed bool     `json:"unattributed,omitempty"`
	Inbound      bool     `json:"inbound,omitempty"`
}

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
		out[i].Addresses = append([]string(nil), s.Addresses...)
		out[i].Groups = append([]string(nil), s.Groups...)
	}
	return out
}

// ParseAccountSelector accepts an exact mailbox, unattributed, or an account
// identity group. Display names, domains, and address substrings are rejected.
func ParseAccountSelector(value string, inbound bool) (AccountScope, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	s := AccountScope{Inbound: inbound}
	if value == "unattributed" {
		s.Unattributed = true
		return s, nil
	}
	if !inbound && strings.HasPrefix(value, "fastmail-masked:") {
		account := strings.TrimPrefix(value, "fastmail-masked:")
		if account == "" || strings.ContainsAny(account, " \t\r\n") {
			return s, errors.New("expected fastmail-masked:<account>")
		}
		s.Groups = []string{value}
		return s, nil
	}
	a, err := mail.ParseAddress(value)
	if err != nil || a.Address != value || !strings.Contains(value, "@") || strings.HasPrefix(value, "@") {
		return s, errors.New("expected an exact email address or unattributed")
	}
	s.Addresses = []string{value}
	return s, nil
}

func accountOperator(inbound bool) operatorFn {
	return func(q *Query, value string, _ time.Time) error {
		s, err := ParseAccountSelector(value, inbound)
		if err != nil {
			return err
		}
		if i, ok := q.accountOperatorScopeIndexes[inbound]; ok {
			q.AccountScopes[i].Addresses = append(q.AccountScopes[i].Addresses, s.Addresses...)
			q.AccountScopes[i].Groups = append(q.AccountScopes[i].Groups, s.Groups...)
			q.AccountScopes[i].Unattributed = q.AccountScopes[i].Unattributed || s.Unattributed
			return nil
		}
		if q.accountOperatorScopeIndexes == nil {
			q.accountOperatorScopeIndexes = make(map[bool]int)
		}
		q.accountOperatorScopeIndexes[inbound] = len(q.AccountScopes)
		q.AccountScopes = append(q.AccountScopes, s)
		return nil
	}
}

// AppendAccountConditions uses trusted caller-owned SQL identifiers. Values
// always bind as parameters. Every predicate runs before pagination/aggregation.
func AppendAccountConditions(conditions []string, args []any, scopes []AccountScope, alias, groupTable string) ([]string, []any) {
	for _, s := range scopes {
		if s.SourceID != nil {
			conditions = append(conditions, alias+".source_id=?")
			args = append(args, *s.SourceID)
		}
		var alternatives []string
		if len(s.Addresses) > 0 {
			marks := make([]string, len(s.Addresses))
			for i, a := range s.Addresses {
				marks[i] = "?"
				args = append(args, strings.ToLower(strings.TrimSpace(a)))
			}
			alternatives = append(alternatives, alias+".account_address IN ("+strings.Join(marks, ",")+")")
		}
		if len(s.Groups) > 0 {
			marks := make([]string, len(s.Groups))
			for i, g := range s.Groups {
				marks[i] = "?"
				args = append(args, strings.ToLower(strings.TrimSpace(g)))
			}
			alternatives = append(alternatives, "EXISTS (SELECT 1 FROM "+groupTable+" ag WHERE ag.source_id="+alias+".source_id AND ag.address_key="+alias+".account_address AND ag.address_key<>'' AND ag.group_key IN ("+strings.Join(marks, ",")+"))")
		}
		if s.Unattributed {
			alternatives = append(alternatives, alias+".account_address IS NULL")
		}
		if len(alternatives) == 0 {
			conditions = append(conditions, "FALSE")
			continue
		}
		path := alias + ".account_path IN ('inbound','sent','calendar')"
		if s.Inbound {
			path = alias + ".account_path='inbound'"
		}
		if s.Unattributed && !s.Inbound {
			path = "(" + path + " OR (" + alias + ".account_attribution_basis='not-derived' AND (" + alias + ".message_type IN ('email','calendar_event') OR " + alias + ".message_type IS NULL OR " + alias + ".message_type='')))"
		}
		conditions = append(conditions, "("+path+" AND ("+strings.Join(alternatives, " OR ")+"))")
	}
	return conditions, args
}

// ValidateAccountScopes validates structured filters before SQL construction.
func ValidateAccountScopes(scopes []AccountScope) error {
	if len(scopes) > 16 {
		return errors.New("too many account scope intersections (maximum 16)")
	}
	for _, s := range scopes {
		if s.SourceID != nil && *s.SourceID <= 0 {
			return errors.New("invalid account scope source ID")
		}
		if len(s.Addresses)+len(s.Groups) > 64 {
			return errors.New("too many account selectors; use an identity group")
		}
		if len(s.Addresses)+len(s.Groups) == 0 && !s.Unattributed {
			return errors.New("empty account scope")
		}
		for _, a := range s.Addresses {
			parsed, err := ParseAccountSelector(a, true)
			if err != nil || len(parsed.Addresses) != 1 {
				return fmt.Errorf("invalid account address %q", a)
			}
		}
		for _, g := range s.Groups {
			parsed, err := ParseAccountSelector(g, s.Inbound)
			if err != nil || len(parsed.Groups) != 1 {
				return fmt.Errorf("invalid identity group %q", g)
			}
		}
	}
	return nil
}

func structuredAccountOperator(q *Query, value string, _ time.Time) error {
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return errors.New("invalid account scope encoding")
	}
	var scopes []AccountScope
	if err = json.Unmarshal(data, &scopes, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("invalid account scopes: %w", err)
	}
	if err = ValidateAccountScopes(scopes); err != nil {
		return err
	}
	q.AccountScopes = append(q.AccountScopes, scopes...)
	return nil
}

func FormatAccountScopes(scopes []AccountScope) []string {
	var counts [2]int
	for _, s := range scopes {
		i := 0
		if s.Inbound {
			i = 1
		}
		counts[i]++
	}
	scoped := false
	for _, s := range scopes {
		scoped = scoped || s.SourceID != nil
	}
	if counts[0] > 1 || counts[1] > 1 || scoped {
		data, _ := json.Marshal(scopes, json.Deterministic(true))
		return []string{"account_scope:" + base64.RawURLEncoding.EncodeToString(data)}
	}
	var parts []string
	for _, s := range scopes {
		op := "account"
		if s.Inbound {
			op = "received"
		}
		parts = appendSearchOperators(parts, op, s.Addresses)
		parts = appendSearchOperators(parts, op, s.Groups)
		if s.Unattributed {
			parts = append(parts, op+":unattributed")
		}
	}
	return parts
}
