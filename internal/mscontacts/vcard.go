package mscontacts

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/vcard"
)

// contact holds the Graph contact fields that map to vCard. The JSON form is
// also the write body, so every mapped field is always sent and an empty
// value clears the field in Outlook.
type contact struct {
	ID              string                        `json:"id,omitzero"`
	ETag            string                        `json:"@odata.etag,omitzero"`
	ParentFolderID  string                        `json:"parentFolderId,omitzero"`
	DisplayName     string                        `json:"displayName"`
	GivenName       string                        `json:"givenName"`
	MiddleName      string                        `json:"middleName"`
	Surname         string                        `json:"surname"`
	Title           string                        `json:"title"`
	Generation      string                        `json:"generation"`
	NickName        string                        `json:"nickName"`
	EmailAddresses  []emailAddress                `json:"emailAddresses"`
	BusinessPhones  []string                      `json:"businessPhones"`
	HomePhones      []string                      `json:"homePhones"`
	MobilePhone     string                        `json:"mobilePhone"`
	CompanyName     string                        `json:"companyName"`
	Department      string                        `json:"department"`
	JobTitle        string                        `json:"jobTitle"`
	Birthday        *string                       `json:"birthday"`
	PersonalNotes   string                        `json:"personalNotes"`
	HomeAddress     physicalAddress               `json:"homeAddress"`
	BusinessAddress physicalAddress               `json:"businessAddress"`
	OtherAddress    physicalAddress               `json:"otherAddress"`
	Properties      []singleValueExtendedProperty `json:"singleValueExtendedProperties,omitzero"`

	// untypedPhones are vCard TEL values without a home, work or cell TYPE.
	// placePhones assigns them to Graph fields.
	untypedPhones []string `json:"-"`
}

type emailAddress struct {
	Name    string `json:"name,omitzero"`
	Address string `json:"address"`
}

type physicalAddress struct {
	Street          string `json:"street,omitzero"`
	City            string `json:"city,omitzero"`
	State           string `json:"state,omitzero"`
	PostalCode      string `json:"postalCode,omitzero"`
	CountryOrRegion string `json:"countryOrRegion,omitzero"`
}

type singleValueExtendedProperty struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

func (a physicalAddress) empty() bool { return a == physicalAddress{} }

// mappedProperties are the vCard properties that Graph holds as fields.
var mappedProperties = []string{"VERSION", "UID", "FN", "N", "NICKNAME", "EMAIL", "TEL", "ORG", "TITLE", "ADR", "BDAY", "NOTE"}

// property returns an extended property value, or "".
func (c contact) property(id string) string {
	for _, property := range c.Properties {
		if strings.EqualFold(property.ID, id) {
			return property.Value
		}
	}
	return ""
}

// uid returns the msgvault UID property, or "" for a contact made in Outlook.
func (c contact) uid() string { return c.property(uidProperty) }

// fields returns only the mapped fields, with empty lists made nil, emails
// without names and the birthday cut to its date, so two contacts compare by
// content. Graph names an email after its address when no name is sent.
func (c contact) fields() contact {
	c.ID, c.ETag, c.ParentFolderID, c.Properties = "", "", "", nil
	emails := make([]emailAddress, 0, len(c.EmailAddresses))
	for _, email := range c.EmailAddresses {
		emails = append(emails, emailAddress{Address: email.Address})
	}
	c.EmailAddresses = emails
	for _, list := range []*[]string{&c.BusinessPhones, &c.HomePhones} {
		if len(*list) == 0 {
			*list = nil
		}
	}
	if len(c.EmailAddresses) == 0 {
		c.EmailAddresses = nil
	}
	if c.Birthday != nil && len(*c.Birthday) >= len("2006-01-02") {
		date := (*c.Birthday)[:len("2006-01-02")]
		c.Birthday = &date
	}
	return c
}

// body returns the vCard for a contact. A contact that msgvault wrote keeps
// the full vCard it was sent. While Outlook's fields still match that vCard,
// the vCard is returned as it was sent, so a publication reads back
// unchanged. After an edit in Outlook, the fields come from Outlook and the
// properties that Graph cannot hold come from the saved vCard.
func (c contact) body(uid string) ([]byte, error) {
	saved := c.property(vcardProperty)
	if saved == "" {
		return c.toVCard(uid, nil)
	}
	sent, err := contactFromVCard([]byte(saved))
	placePhones(&sent, &c)
	if err == nil && reflect.DeepEqual(sent.fields(), c.fields()) {
		return []byte(saved), nil
	}
	document, err := vcard.Decode(strings.NewReader(saved))
	if err != nil || len(document.Cards) != 1 {
		return c.toVCard(uid, nil)
	}
	var extra []vcard.Property
	for _, property := range document.Cards[0].Properties {
		if !slices.Contains(mappedProperties, strings.ToUpper(property.Name)) {
			extra = append(extra, property)
		}
	}
	return c.toVCard(uid, extra)
}

// toVCard renders the contact as a vCard 4.0 card with uid as its UID, and
// appends extra properties.
func (c contact) toVCard(uid string, extra []vcard.Property) ([]byte, error) {
	card := vcard.Card{}
	add := func(name, raw string, types ...string) error {
		property, err := vcard.NewProperty("", name, raw)
		if err != nil {
			return err
		}
		if len(types) > 0 {
			parameter, err := vcard.NewParameter("TYPE", types...)
			if err != nil {
				return err
			}
			property.Parameters = append(property.Parameters, parameter)
		}
		card.Properties = append(card.Properties, property)
		return nil
	}
	fullName := strings.TrimSpace(c.DisplayName)
	if fullName == "" {
		fullName = strings.Join(strings.Fields(strings.Join([]string{c.GivenName, c.MiddleName, c.Surname}, " ")), " ")
	}
	errs := []error{
		add("VERSION", "4.0"),
		add("UID", vcard.EscapeText(uid)),
		add("FN", vcard.EscapeText(fullName)),
	}
	if c.Surname+c.GivenName+c.MiddleName+c.Title+c.Generation != "" {
		errs = append(errs, add("N", vcard.JoinStructuredText([]string{c.Surname, c.GivenName, c.MiddleName, c.Title, c.Generation})))
	}
	if c.NickName != "" {
		errs = append(errs, add("NICKNAME", vcard.EscapeText(c.NickName)))
	}
	for _, email := range c.EmailAddresses {
		if address := strings.TrimSpace(email.Address); address != "" {
			errs = append(errs, add("EMAIL", vcard.EscapeText(address)))
		}
	}
	for _, phone := range c.BusinessPhones {
		errs = append(errs, add("TEL", vcard.EscapeText(phone), "work"))
	}
	for _, phone := range c.HomePhones {
		errs = append(errs, add("TEL", vcard.EscapeText(phone), "home"))
	}
	if c.MobilePhone != "" {
		errs = append(errs, add("TEL", vcard.EscapeText(c.MobilePhone), "cell"))
	}
	if c.CompanyName+c.Department != "" {
		errs = append(errs, add("ORG", vcard.JoinStructuredText(vcard.OrganizationComponents(c.CompanyName, c.Department))))
	}
	if c.JobTitle != "" {
		errs = append(errs, add("TITLE", vcard.EscapeText(c.JobTitle)))
	}
	for _, address := range []struct {
		value physicalAddress
		kind  []string
	}{{c.HomeAddress, []string{"home"}}, {c.BusinessAddress, []string{"work"}}, {c.OtherAddress, nil}} {
		if !address.value.empty() {
			a := address.value
			errs = append(errs, add("ADR", vcard.JoinStructuredText([]string{"", "", a.Street, a.City, a.State, a.PostalCode, a.CountryOrRegion}), address.kind...))
		}
	}
	if c.Birthday != nil && len(*c.Birthday) >= len("2006-01-02") {
		errs = append(errs, add("BDAY", strings.ReplaceAll((*c.Birthday)[:len("2006-01-02")], "-", "")))
	}
	if c.PersonalNotes != "" {
		errs = append(errs, add("NOTE", vcard.EscapeText(c.PersonalNotes)))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("render Graph contact as vCard: %w", err)
	}
	card.Properties = append(card.Properties, extra...)
	return vcard.Marshal(vcard.Document{Cards: []vcard.Card{card}})
}

// keepEmailNames copies the Outlook display name of each email address that
// current already holds, because vCard has no field for it.
func keepEmailNames(c, current *contact) {
	for i, email := range c.EmailAddresses {
		for _, existing := range current.EmailAddresses {
			if strings.EqualFold(existing.Address, email.Address) {
				c.EmailAddresses[i].Name = existing.Name
				break
			}
		}
	}
}

// placePhones assigns the phones of c that have no TYPE. A number that
// current already holds keeps its Outlook field; any other number becomes a
// business phone.
func placePhones(c, current *contact) {
	digits := func(number string) string {
		return strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, number)
	}
	for _, number := range c.untypedPhones {
		key := digits(number)
		switch {
		case current != nil && c.MobilePhone == "" && digits(current.MobilePhone) == key:
			c.MobilePhone = number
		case current != nil && slices.ContainsFunc(current.HomePhones, func(home string) bool { return digits(home) == key }):
			c.HomePhones = append(c.HomePhones, number)
		default:
			c.BusinessPhones = append(c.BusinessPhones, number)
		}
	}
	c.untypedPhones = nil
}

// contactFromVCard maps a vCard to the Graph contact fields. Properties that
// Graph cannot hold are dropped.
func contactFromVCard(body []byte) (contact, error) {
	document, err := vcard.Decode(strings.NewReader(string(body)))
	if err != nil {
		return contact{}, fmt.Errorf("parse vCard for Graph: %w", err)
	}
	if len(document.Cards) != 1 {
		return contact{}, errors.New("graph contact needs exactly one vCard")
	}
	card := document.Cards[0]
	c := contact{EmailAddresses: []emailAddress{}, BusinessPhones: []string{}, HomePhones: []string{}}
	text := func(property vcard.Property) string {
		value, err := vcard.UnescapeText(property.RawValue)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(value)
	}
	parts := func(property vcard.Property, n int) []string {
		values, err := vcard.SplitStructuredText(property.RawValue)
		if err != nil {
			values = nil
		}
		for len(values) < n {
			values = append(values, "")
		}
		return values
	}
	types := func(property vcard.Property) []string {
		var values []string
		for _, parameter := range property.ParametersNamed("TYPE") {
			for _, value := range parameter.Values {
				for part := range strings.SplitSeq(value.Decoded, ",") {
					values = append(values, strings.ToLower(strings.TrimSpace(part)))
				}
			}
		}
		return values
	}
	for _, property := range card.Properties {
		switch strings.ToUpper(property.Name) {
		case "FN":
			if c.DisplayName == "" {
				c.DisplayName = text(property)
			}
		case "N":
			n := parts(property, 5)
			c.Surname, c.GivenName, c.MiddleName, c.Title, c.Generation = n[0], n[1], n[2], n[3], n[4]
		case "NICKNAME":
			if c.NickName == "" {
				c.NickName = text(property)
			}
		case "EMAIL":
			if address := strings.TrimPrefix(text(property), "mailto:"); address != "" {
				c.EmailAddresses = append(c.EmailAddresses, emailAddress{Address: address})
			}
		case "TEL":
			number := strings.TrimPrefix(text(property), "tel:")
			if number == "" {
				continue
			}
			switch kinds := types(property); {
			case slices.Contains(kinds, "cell") && c.MobilePhone == "":
				c.MobilePhone = number
			case slices.Contains(kinds, "home"):
				c.HomePhones = append(c.HomePhones, number)
			case slices.Contains(kinds, "cell") || slices.Contains(kinds, "work"):
				c.BusinessPhones = append(c.BusinessPhones, number)
			default:
				c.untypedPhones = append(c.untypedPhones, number)
			}
		case "ORG":
			org := parts(property, 2)
			c.CompanyName, c.Department = org[0], org[1]
		case "TITLE":
			c.JobTitle = text(property)
		case "NOTE":
			c.PersonalNotes = text(property)
		case "BDAY":
			date, err := vcard.ParsePartialDate(strings.TrimSpace(property.RawValue))
			if err == nil && date.Year != nil && date.Month != nil && date.Day != nil {
				birthday := fmt.Sprintf("%04d-%02d-%02dT00:00:00Z", *date.Year, *date.Month, *date.Day)
				c.Birthday = &birthday
			}
		case "ADR":
			adr := parts(property, 7)
			address := physicalAddress{Street: adr[2], City: adr[3], State: adr[4], PostalCode: adr[5], CountryOrRegion: adr[6]}
			kinds := types(property)
			switch {
			case slices.Contains(kinds, "home") && c.HomeAddress.empty():
				c.HomeAddress = address
			case slices.Contains(kinds, "work") && c.BusinessAddress.empty():
				c.BusinessAddress = address
			case c.OtherAddress.empty():
				c.OtherAddress = address
			}
		}
	}
	return c, nil
}
