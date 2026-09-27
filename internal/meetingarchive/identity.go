package meetingarchive

import (
	"strings"
	"unicode"
)

// Normalized returns the person with trimmed, lowercased emails, E.164-only
// phones, invalid values dropped, and extra identities de-duplicated against
// the primary ones. The name is presentation data only.
func (p Person) Normalized() Person {
	out := Person{
		Name:   strings.TrimSpace(p.Name),
		Email:  normalizePersonEmail(p.Email),
		Phone:  normalizePersonPhone(p.Phone),
		Anchor: strings.TrimSpace(p.Anchor),
	}
	seenEmails := map[string]bool{out.Email: true}
	for _, email := range p.OtherEmails {
		if email = normalizePersonEmail(email); email != "" && !seenEmails[email] {
			seenEmails[email] = true
			out.OtherEmails = append(out.OtherEmails, email)
		}
	}
	seenPhones := map[string]bool{out.Phone: true}
	for _, phone := range p.OtherPhones {
		if phone = normalizePersonPhone(phone); phone != "" && !seenPhones[phone] {
			seenPhones[phone] = true
			out.OtherPhones = append(out.OtherPhones, phone)
		}
	}
	return out
}

// PrimaryKey names the identity that makes this person a recipient: the
// email when present, otherwise the phone. Empty means the person has no
// usable identity.
func (p Person) PrimaryKey() string {
	switch {
	case p.Email != "":
		return "email:" + p.Email
	case p.Phone != "":
		return "phone:" + p.Phone
	default:
		return ""
	}
}

// identities lists every email and phone of a normalized person, primary
// identities first.
func (p Person) identities() []identity {
	var out []identity
	if p.Email != "" {
		out = append(out, identity{kind: identityEmail, value: p.Email})
	}
	if p.Phone != "" {
		out = append(out, identity{kind: identityPhone, value: p.Phone})
	}
	for _, email := range p.OtherEmails {
		out = append(out, identity{kind: identityEmail, value: email})
	}
	for _, phone := range p.OtherPhones {
		out = append(out, identity{kind: identityPhone, value: phone})
	}
	return out
}

type identityKind string

const (
	identityEmail identityKind = "email"
	identityPhone identityKind = "phone"
)

type identity struct {
	kind  identityKind
	value string
}

// normalizePersonEmail keeps the archiver's historical leniency (providers
// validate their own addresses) while refusing values that cannot be an
// address at all.
func normalizePersonEmail(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if !strings.Contains(value, "@") || strings.IndexFunc(value, unicode.IsSpace) >= 0 {
		return ""
	}
	return value
}

// normalizePersonPhone accepts only E.164 values. Providers normalize their
// own formats before building a snapshot; guessing a country here could
// attach a meeting to the wrong person.
func normalizePersonPhone(value string) string {
	value = strings.TrimSpace(value)
	if len(value) < 8 || len(value) > 16 || value[0] != '+' || value[1] == '0' {
		return ""
	}
	for _, r := range value[1:] {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return value
}
