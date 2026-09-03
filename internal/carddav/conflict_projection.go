package carddav

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vcard"
)

const (
	maxPublicCardDAVValueBytes = 256
	maxPublicContactValues     = 8
)

type ConflictSideState string

const (
	ConflictSidePresent     ConflictSideState = "present"
	ConflictSideDeleted     ConflictSideState = "deleted"
	ConflictSideUnavailable ConflictSideState = "unavailable"
)

type ContactSummary struct {
	State       ConflictSideState `json:"state"`
	DisplayName string            `json:"display_name,omitempty"`
	Emails      []string          `json:"emails"`
	Phones      []string          `json:"phones"`
	Truncated   bool              `json:"truncated,omitempty"`
}

type AddressBookIdentity struct {
	ID   int64
	Name string
}

type ConflictListItem struct {
	ID                 int64
	AddressBook        AddressBookIdentity
	Status             store.CardDAVConflictStatus
	LocalState         ConflictSideState
	RemoteState        ConflictSideState
	AllowedResolutions []ResolutionChoice
	UpdatedAt          time.Time
}

type ConflictDetail struct {
	ID                 int64
	AddressBook        AddressBookIdentity
	Status             store.CardDAVConflictStatus
	Resolution         store.CardDAVConflictResolution
	Base               ContactSummary
	Local              ContactSummary
	Remote             ContactSummary
	AllowedResolutions []ResolutionChoice
	CreatedAt          time.Time
	UpdatedAt          time.Time
	ResolvedAt         *time.Time
}

func publicAddressBookIdentity(id int64, name string) AddressBookIdentity {
	name = normalizePublicCardDAVText(name)
	name, _ = capPublicCardDAVText(name)
	return AddressBookIdentity{ID: id, Name: name}
}

func emptyContactSummary(state ConflictSideState) ContactSummary {
	return ContactSummary{State: state, Emails: []string{}, Phones: []string{}}
}

func projectConflictContact(body []byte, tombstone bool) ContactSummary {
	if tombstone {
		return emptyContactSummary(ConflictSideDeleted)
	}
	envelope, err := vcard.ParseResourceEnvelope(body)
	if err != nil {
		return emptyContactSummary(ConflictSideUnavailable)
	}
	summary := emptyContactSummary(ConflictSidePresent)
	emails := make(map[string]struct{})
	phones := make(map[string]struct{})
	for _, occurrence := range envelope.PropertyTree {
		property := occurrence.Property
		name := strings.ToUpper(property.Name)
		if name != "FN" && name != "EMAIL" && name != "TEL" {
			continue
		}
		value, err := cardDAVPropertyValue(envelope.RenderMetadata.StoredVersion, property)
		if err != nil {
			return emptyContactSummary(ConflictSideUnavailable)
		}
		switch name {
		case "EMAIL":
			value = trimPrefixFold(strings.TrimSpace(value), "mailto:")
		case "TEL":
			value = trimPrefixFold(strings.TrimSpace(value), "tel:")
		}
		value = normalizePublicCardDAVText(value)
		if value == "" {
			continue
		}
		if name == "FN" && summary.DisplayName != "" {
			continue
		}
		dedupeKey := value
		clipped, truncated := capPublicCardDAVText(value)
		summary.Truncated = summary.Truncated || truncated
		switch name {
		case "FN":
			if summary.DisplayName == "" {
				summary.DisplayName = clipped
			}
		case "EMAIL":
			if _, exists := emails[dedupeKey]; exists {
				continue
			}
			if len(summary.Emails) == maxPublicContactValues {
				summary.Truncated = true
				continue
			}
			emails[dedupeKey] = struct{}{}
			summary.Emails = append(summary.Emails, clipped)
		case "TEL":
			if _, exists := phones[dedupeKey]; exists {
				continue
			}
			if len(summary.Phones) == maxPublicContactValues {
				summary.Truncated = true
				continue
			}
			phones[dedupeKey] = struct{}{}
			summary.Phones = append(summary.Phones, clipped)
		}
	}
	return summary
}

func normalizePublicCardDAVText(value string) string {
	var cleaned strings.Builder
	cleaned.Grow(len(value))
	for _, r := range strings.ToValidUTF8(value, "") {
		if unicode.IsSpace(r) {
			cleaned.WriteByte(' ')
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		cleaned.WriteRune(r)
	}
	return strings.Join(strings.Fields(cleaned.String()), " ")
}

func capPublicCardDAVText(value string) (string, bool) {
	if len(value) <= maxPublicCardDAVValueBytes {
		return value, false
	}
	end := maxPublicCardDAVValueBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end], true
}
