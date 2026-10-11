package mscontacts

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"unicode"

	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/vcard"
)

// contact holds the Graph contact fields that map to vCard. The JSON form is
// also the write body, so every mapped field is always sent and an empty
// value clears the field in Outlook.
type contact struct {
	ID              string                        `json:"id,omitzero"`
	ETag            string                        `json:"@odata.etag,omitzero"`
	ParentFolderID  string                        `json:"parentFolderId,omitzero"`
	CreatedDateTime string                        `json:"createdDateTime,omitzero"`
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
	Categories      []string                      `json:"categories"`
	HomeAddress     physicalAddress               `json:"homeAddress"`
	BusinessAddress physicalAddress               `json:"businessAddress"`
	OtherAddress    physicalAddress               `json:"otherAddress"`
	Properties      []singleValueExtendedProperty `json:"singleValueExtendedProperties,omitzero"`

	// untypedPhones are vCard TEL values without a home, work or cell TYPE.
	// placePhones assigns them to Graph fields.
	untypedPhones []string `json:"-"`
	// sources records the saved line of each value, for overflowLines only.
	sources *lineSources `json:"-"`
}

// lineSources holds the index of the saved line behind each value, parallel
// to the Graph lists. held covers the mobile phone and the three addresses,
// which Graph never cuts.
type lineSources struct{ emails, business, home, untyped, held []int }

type emailAddress struct {
	Name    string `json:"name,omitzero"`
	Address string `json:"address"`
}

// physicalAddress is always sent whole, because Graph requires the full
// property set when it updates an address.
type physicalAddress struct {
	Street          string `json:"street"`
	City            string `json:"city"`
	State           string `json:"state"`
	PostalCode      string `json:"postalCode"`
	CountryOrRegion string `json:"countryOrRegion"`
}

type singleValueExtendedProperty struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

func (a physicalAddress) empty() bool { return a == physicalAddress{} }

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
	c.ID, c.ETag, c.ParentFolderID, c.CreatedDateTime, c.Properties = "", "", "", "", nil
	c = c.vcardText()
	emails := make([]emailAddress, 0, len(c.EmailAddresses))
	for _, email := range c.EmailAddresses {
		emails = append(emails, emailAddress{Address: email.Address})
	}
	c.EmailAddresses = emails
	for _, list := range []*[]string{&c.BusinessPhones, &c.HomePhones, &c.Categories} {
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

// lastWrite is msgvault's last write to a contact, decoded: the saved vCard
// and the Graph fields as sent.
type lastWrite struct {
	document vcard.Document
	sent     contact
}

// lastWrite decodes the saved vCard and sent fields, if both are readable.
func (c contact) lastWrite() (lastWrite, bool) {
	saved := c.property(vcardProperty)
	if saved == "" {
		return lastWrite{}, false
	}
	document, err := vcard.Decode(strings.NewReader(saved))
	if err != nil || len(document.Cards) != 1 {
		return lastWrite{}, false
	}
	sent, err := c.sent()
	if err != nil {
		return lastWrite{}, false
	}
	return lastWrite{document: document, sent: sent}, true
}

// marked reports a contact that msgvault wrote.
func (c contact) marked() bool {
	return c.uid() != "" || c.property(sentProperty) != ""
}

// thin reports a contact that msgvault wrote whose saved vCard or sent
// fields are absent or unreadable, as a listing may return a contact with a
// large one. body refuses exactly these.
func (c contact) thin() bool {
	_, ok := c.lastWrite()
	return c.marked() && !ok
}

// body returns the vCard for a contact. A contact that msgvault wrote keeps
// the full vCard it was sent. While Outlook's fields still match what was
// sent, that vCard is returned as sent, so a publication reads back
// unchanged. After an edit in Outlook, Outlook's fields win. A value still in
// the field it was written to keeps its saved line. The saved vCard adds only
// what Outlook never held: the properties Graph has no field for, and the
// EMAIL, TEL and ADR lines the write did not send, such as a fourth email or
// a fax.
func (c contact) body(uid string) ([]byte, error) {
	if c.thin() {
		// A thin card would read as an Outlook edit of every saved line.
		return nil, fmt.Errorf("%w: graph contact %s lacks a readable saved vCard and sent fields", carddav.ErrIncompleteMultiget, c.ID)
	}
	write, ok := c.lastWrite()
	if !ok {
		return c.toVCard(uid)
	}
	saved, sent := c.property(vcardProperty), write.sent
	lines := write.document.Cards[0].Properties
	if cardUID(write.document.Cards[0]) != uid {
		// An Outlook copy carries its original's saved vCard. The copy keeps
		// that data under its own UID.
		lines = slices.Clone(lines)
		for i, property := range lines {
			if strings.EqualFold(property.Name, "UID") {
				lines[i].RawValue = vcard.EscapeText(uid)
			}
		}
		rewritten, err := vcard.Marshal(vcard.Document{Cards: []vcard.Card{{Properties: lines}}})
		if err != nil {
			return c.toVCard(uid)
		}
		saved = string(rewritten)
	}
	if reflect.DeepEqual(sent.fields(), c.fields()) {
		return []byte(saved), nil
	}
	sentCard, err := sent.card(uid)
	if err != nil {
		return c.toVCard(uid)
	}
	card, err := c.card(uid)
	if err != nil {
		return nil, err
	}
	overflows := overflowLines(lines, sent)
	unmoved := map[string]int{}
	for _, property := range sentCard.Properties {
		unmoved[fieldKey(property)]++
	}
	// shown holds the lines Outlook now has in a field the write did not
	// fill, so an overflow line that Outlook took up is not added twice.
	shown, sentFields := map[string][]vcard.Property{}, maps.Clone(unmoved)
	for _, property := range card.Properties {
		if field := fieldKey(property); sentFields[field] > 0 {
			sentFields[field]--
		} else {
			shown[lineKey(property)] = append(shown[lineKey(property)], property)
		}
	}
	// Saved lines go back as saved while the card still maps to Outlook's
	// fields. Otherwise only those whose labels pick the field Outlook shows
	// go back, for example after Outlook cleared the first of two mobiles.
	restored := slices.Clone(card.Properties)
	restoreUnmoved(restored, lines, overflows, maps.Clone(unmoved), false)
	if c.mapsTo(restored) {
		card.Properties = restored
	} else {
		restoreUnmoved(card.Properties, lines, overflows, unmoved, true)
	}
	for i, property := range lines {
		name := strings.ToUpper(property.Name)
		// Each Outlook value in the same kind of field hides one overflow line.
		// Outlook can't hold a fax, so nothing hides one.
		overflow := overflows[i]
		if key := lineKey(property); overflow && (name != "TEL" || !noPhoneField(property)) {
			if k := slices.IndexFunc(shown[key], func(rendered vcard.Property) bool { return fieldsMatch(property, rendered) }); k >= 0 {
				shown[key], overflow = slices.Delete(shown[key], k, k+1), false
			}
		}
		// A card holds one birthday, so Outlook's own wins over a kept one.
		kept := keptProperty(property) && (name != "BDAY" || len(card.PropertiesNamed("BDAY")) == 0)
		if overflow || kept {
			card.Properties = append(card.Properties, property)
		}
	}
	return vcard.Marshal(vcard.Document{Cards: []vcard.Card{card}})
}

// saved returns the extended properties that every write sets: the vCard
// as sent and the Graph fields as sent.
func (c contact) saved(body []byte) []singleValueExtendedProperty {
	properties := []singleValueExtendedProperty{{ID: vcardProperty, Value: string(body)}}
	if sent, err := json.Marshal(c.fields()); err == nil {
		properties = append(properties, singleValueExtendedProperty{ID: sentProperty, Value: string(sent)})
	}
	return properties
}

// sent returns the Graph fields of the last write, which every write saves.
func (c contact) sent() (contact, error) {
	var sent contact
	err := json.Unmarshal([]byte(c.property(sentProperty)), &sent)
	return sent, err
}

// overflowLines maps the saved lines as the write did and reports the EMAIL,
// TEL and ADR lines whose values the write did not keep. The write placed
// untyped phones against the Outlook contact it replaced, or none on a
// create, so sent stands in for that contact first and nil second. When
// neither reproduces sent, every TEL line counts as overflow, so a phone may
// come back twice but never drops.
func overflowLines(lines []vcard.Property, sent contact) []bool {
	kept := make([]bool, len(lines))
	var s lineSources
	for _, current := range []*contact{&sent, nil} {
		s = lineSources{}
		c, _ := mapCard(vcard.Card{Properties: lines}, &s)
		placePhones(&c, current)
		c.sources = nil
		if reflect.DeepEqual(c.fields(), sent.fields()) {
			for _, i := range slices.Concat(s.business, s.home, s.held) {
				kept[i] = true
			}
			break
		}
	}
	for _, i := range s.emails {
		kept[i] = true
	}
	for _, i := range s.held {
		kept[i] = kept[i] || strings.EqualFold(lines[i].Name, "ADR")
	}
	overflow := make([]bool, len(lines))
	for i, line := range lines {
		overflow[i] = slices.Contains([]string{"EMAIL", "TEL", "ADR"}, strings.ToUpper(line.Name)) && !kept[i]
	}
	return overflow
}

// restoreUnmoved replaces each rendered property whose value is still in
// the Graph field it was written to, as counted in unmoved, with the saved
// line, so its form and parameters survive, for example a PO box or
// EMAIL;TYPE=work. An overflow line never stands in for a rendered value;
// body appends it itself.
func restoreUnmoved(rendered, saved []vcard.Property, overflow []bool, unmoved map[string]int, strict bool) {
	used := slices.Clone(overflow)
	for i, property := range rendered {
		field := fieldKey(property)
		if unmoved[field] == 0 {
			continue
		}
		var fits func(vcard.Property) bool
		if strict {
			fits = func(old vcard.Property) bool { return fieldsMatch(old, property) }
		}
		if j := savedMatch(property, saved, used, fits); j >= 0 {
			used[j], rendered[i] = true, saved[j]
			unmoved[field]--
		}
	}
}

// fieldsMatch reports whether a saved TEL or ADR line maps to the Graph field
// that rendered shows, so restoring it keeps the value in that field. Its
// cell, home and work types must all be on rendered; an untyped phone keeps
// its current field when published.
func fieldsMatch(saved, rendered vcard.Property) bool {
	if name := strings.ToUpper(rendered.Name); name != "TEL" && name != "ADR" {
		return true
	}
	shown := typeValues(rendered)
	return !slices.ContainsFunc(typeValues(saved), func(kind string) bool {
		return slices.Contains([]string{"cell", "home", "work"}, kind) && !slices.Contains(shown, kind)
	})
}

// graphProperties are the properties that mapVCard maps to Graph fields, so
// Outlook decides them after an edit, and the lines card writes itself.
// TestGraphPropertiesCoverTheMapping fails when a mapping is missing here.
var graphProperties = []string{
	"FN", "N", "NICKNAME", "EMAIL", "TEL", "ORG", "TITLE", "BDAY", "NOTE", "ADR", "CATEGORIES",
	"VERSION", "UID", "PRODID", "REV",
}

// mapsTo reports whether a card with these properties maps to the Graph
// fields of c, so publishing it would leave each value in its field.
func (c contact) mapsTo(properties []vcard.Property) bool {
	body, err := vcard.Marshal(vcard.Document{Cards: []vcard.Card{{Properties: properties}}})
	if err != nil {
		return false
	}
	mapped, _, err := mapVCard(body)
	if err != nil {
		return false
	}
	placePhones(&mapped, &c)
	return reflect.DeepEqual(mapped.fields(), c.fields())
}

// keptProperty reports a property that Graph has no field for, which the
// saved vCard keeps after an Outlook edit, such as a photo, a birthday
// without a year or a phonetic name. Keeping one never grows a write, which
// always saves the outgoing vCard.
func keptProperty(property vcard.Property) bool {
	name := strings.ToUpper(property.Name)
	if (name == "BDAY" && !fullDate(property)) || (name == "N" && len(property.ParametersNamed("PHONETIC")) > 0) {
		return true
	}
	return !slices.Contains(graphProperties, name)
}

// lineKey returns the property name and the part of its value that Graph
// holds: the address of an EMAIL, the digits and letters of a TEL, the street to country
// of an ADR, the components of N and ORG, the date of a BDAY, and the text of
// other properties.
func lineKey(property vcard.Property) string {
	name := strings.ToUpper(property.Name)
	raw := property.RawValue
	components := func(from, to int) string {
		values, err := vcard.SplitStructuredText(raw)
		if err != nil {
			return raw
		}
		for len(values) < to {
			values = append(values, "")
		}
		return strings.Join(values[from:to], "\x1f")
	}
	value, err := vcard.UnescapeText(raw)
	if err != nil {
		value = raw
	}
	value = strings.TrimSpace(value)
	switch name {
	case "EMAIL":
		value = strings.ToLower(withoutScheme(value, "mailto:"))
	case "TEL":
		value = phoneKey(value)
	case "ADR":
		value = components(2, 7)
	case "N":
		value = components(0, 5)
	case "ORG":
		company, department := organization(raw)
		value = company + "\x1f" + department
	case "BDAY":
		if fullDate(property) {
			date, _ := vcard.ParsePartialDate(strings.TrimSpace(raw))
			value = fmt.Sprintf("%04d%02d%02d", *date.Year, *date.Month, *date.Day)
		}
	}
	return name + "\x00" + value
}

// fieldKey returns the lineKey of a rendered property and its TYPE, which
// names the Graph field that holds it.
func fieldKey(property vcard.Property) string {
	return lineKey(property) + "\x00" + strings.Join(typeValues(property), ",")
}

// savedMatch returns the first unused saved line that holds the value of
// rendered and that fits accepts, when set, preferring one with every TYPE of
// rendered, so the same number saved in two fields keeps each line. A fax
// line or a phonetic name never matches, since mapVCard gives it no Graph
// field. It returns -1 when none holds the value.
func savedMatch(rendered vcard.Property, saved []vcard.Property, used []bool, fits func(vcard.Property) bool) int {
	key, types := lineKey(rendered), typeValues(rendered)
	match := -1
	for j, old := range saved {
		if used[j] || lineKey(old) != key || (strings.EqualFold(old.Name, "TEL") && noPhoneField(old)) ||
			(strings.EqualFold(old.Name, "N") && len(old.ParametersNamed("PHONETIC")) > 0) ||
			(fits != nil && !fits(old)) {
			continue
		}
		if !slices.ContainsFunc(types, func(kind string) bool { return !slices.Contains(typeValues(old), kind) }) {
			return j
		}
		if match < 0 {
			match = j
		}
	}
	return match
}

// noPhoneField reports a fax, pager or textphone TEL, which has no Graph
// field and stays only in the saved vCard.
func noPhoneField(property vcard.Property) bool {
	return slices.ContainsFunc(typeValues(property), func(kind string) bool {
		return slices.Contains([]string{"fax", "pager", "textphone"}, kind)
	})
}

// cardUID returns the UID of a card, or "".
func cardUID(card vcard.Card) string {
	for _, property := range card.PropertiesNamed("UID") {
		if value, err := vcard.UnescapeText(property.RawValue); err == nil {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// vcardText returns c with LF line ends and without NUL characters in its
// text fields, because Outlook writes CRLF and vCard text cannot hold a CR or
// a NUL.
func (c contact) vcardText() contact {
	c.BusinessPhones, c.HomePhones, c.Categories = slices.Clone(c.BusinessPhones), slices.Clone(c.HomePhones), slices.Clone(c.Categories)
	c.EmailAddresses = slices.Clone(c.EmailAddresses)
	fields := []*string{
		&c.DisplayName, &c.GivenName, &c.MiddleName, &c.Surname, &c.Title, &c.Generation, &c.NickName,
		&c.MobilePhone, &c.CompanyName, &c.Department, &c.JobTitle, &c.PersonalNotes,
	}
	for _, address := range []*physicalAddress{&c.HomeAddress, &c.BusinessAddress, &c.OtherAddress} {
		fields = append(fields, &address.Street, &address.City, &address.State, &address.PostalCode, &address.CountryOrRegion)
	}
	for _, list := range [][]string{c.BusinessPhones, c.HomePhones, c.Categories} {
		for i := range list {
			fields = append(fields, &list[i])
		}
	}
	for i := range c.EmailAddresses {
		fields = append(fields, &c.EmailAddresses[i].Address)
	}
	for _, field := range fields {
		*field = vcardTextReplacer.Replace(*field)
	}
	return c
}

var vcardTextReplacer = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\x00", "")

// organization returns the company and the department of an ORG value. The
// units after the company form one department joined by "/", the inverse of
// vcard.OrganizationComponents, so a department reads back as written.
func organization(raw string) (string, string) {
	values, err := vcard.SplitStructuredText(raw)
	if err != nil || len(values) == 0 {
		return "", ""
	}
	var units []string
	for _, value := range values[1:] {
		for unit := range strings.FieldsFuncSeq(value, func(r rune) bool { return strings.ContainsRune("/>", r) }) {
			if unit = strings.TrimSpace(unit); unit != "" {
				units = append(units, unit)
			}
		}
	}
	return strings.TrimSpace(values[0]), strings.Join(units, "/")
}

// withoutScheme strips a URI scheme such as tel: or mailto: in any case and
// decodes what follows, so tel:%2B1555 maps to +1555.
func withoutScheme(value, scheme string) string {
	if len(value) < len(scheme) || !strings.EqualFold(value[:len(scheme)], scheme) {
		return value
	}
	rest := value[len(scheme):]
	if decoded, err := url.PathUnescape(rest); err == nil {
		return decoded
	}
	return rest
}

// phoneKey returns the digits and letters of a phone number in upper case,
// without a tel: scheme, so equal numbers compare equal in any format.
func phoneKey(number string) string {
	return strings.ToUpper(strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return -1
	}, withoutScheme(strings.TrimSpace(number), "tel:")))
}

// typeValues returns the lower-case TYPE values of a property.
func typeValues(property vcard.Property) []string {
	var values []string
	for _, parameter := range property.ParametersNamed("TYPE") {
		for _, value := range parameter.Values {
			for part := range strings.SplitSeq(value.Decoded, ",") {
				if part = strings.ToLower(strings.TrimSpace(part)); part != "" {
					values = append(values, part)
				}
			}
		}
	}
	return values
}

// toVCard renders the contact as a vCard 4.0 card with uid as its UID.
func (c contact) toVCard(uid string) ([]byte, error) {
	card, err := c.card(uid)
	if err != nil {
		return nil, err
	}
	return vcard.Marshal(vcard.Document{Cards: []vcard.Card{card}})
}

// card builds the vCard 4.0 card for toVCard.
func (c contact) card(uid string) (vcard.Card, error) {
	c = c.vcardText()
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
	// The mobile phone comes first, because contactFromVCard gives Graph's
	// one mobile field to the first cell line.
	if c.MobilePhone != "" {
		errs = append(errs, add("TEL", vcard.EscapeText(c.MobilePhone), "cell"))
	}
	for _, phone := range c.BusinessPhones {
		errs = append(errs, add("TEL", vcard.EscapeText(phone), "work"))
	}
	for _, phone := range c.HomePhones {
		errs = append(errs, add("TEL", vcard.EscapeText(phone), "home"))
	}
	if c.CompanyName+c.Department != "" {
		units := vcard.OrganizationComponents(c.CompanyName, c.Department)
		if len(units) == 0 {
			// A department without a company keeps an empty company.
			units = []string{"", c.Department}
		}
		errs = append(errs, add("ORG", vcard.JoinStructuredText(units)))
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
	for _, category := range c.Categories {
		errs = append(errs, add("CATEGORIES", vcard.EscapeText(category)))
	}
	if err := errors.Join(errs...); err != nil {
		return vcard.Card{}, fmt.Errorf("render Graph contact as vCard: %w", err)
	}
	return card, nil
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
// business phone. It then cuts the lists to Graph's limits, because Graph
// refuses a write that exceeds them. The saved vCard keeps the rest.
func placePhones(c, current *contact) {
	s := c.sources
	if s == nil {
		s = &lineSources{untyped: make([]int, len(c.untypedPhones))}
	}
	for k, number := range c.untypedPhones {
		key := phoneKey(number)
		switch {
		case current != nil && c.MobilePhone == "" && phoneKey(current.MobilePhone) == key:
			c.MobilePhone = number
			s.held = append(s.held, s.untyped[k])
		case current != nil && slices.ContainsFunc(current.HomePhones, func(home string) bool { return phoneKey(home) == key }):
			c.HomePhones = append(c.HomePhones, number)
			s.home = append(s.home, s.untyped[k])
		default:
			c.BusinessPhones = append(c.BusinessPhones, number)
			s.business = append(s.business, s.untyped[k])
		}
	}
	c.untypedPhones, s.untyped = nil, nil
	c.EmailAddresses = c.EmailAddresses[:min(len(c.EmailAddresses), 3)]
	c.BusinessPhones = c.BusinessPhones[:min(len(c.BusinessPhones), 2)]
	c.HomePhones = c.HomePhones[:min(len(c.HomePhones), 2)]
	s.emails = s.emails[:min(len(s.emails), 3)]
	s.business = s.business[:min(len(s.business), 2)]
	s.home = s.home[:min(len(s.home), 2)]
}

// mapVCard maps a vCard to the Graph contact fields and also returns the
// names of the lines Graph has no room for, which an Outlook edit loses: a
// second FN, N, NICKNAME, TITLE, ORG, NOTE or BDAY.
func mapVCard(body []byte) (contact, []string, error) {
	document, err := vcard.Decode(strings.NewReader(string(body)))
	if err != nil {
		return contact{}, nil, fmt.Errorf("parse vCard for Graph: %w", err)
	}
	if len(document.Cards) != 1 {
		return contact{}, nil, errors.New("graph contact needs exactly one vCard")
	}
	c, dropped := mapCard(document.Cards[0], nil)
	return c, dropped, nil
}

// mapCard maps a decoded card as mapVCard does. When sources is set, it
// records the saved line of each value there for placePhones and overflowLines.
func mapCard(card vcard.Card, sources *lineSources) (contact, []string) {
	var dropped []string
	c := contact{EmailAddresses: []emailAddress{}, BusinessPhones: []string{}, HomePhones: []string{}, Categories: []string{}, sources: sources}
	s := sources
	if s == nil {
		s = &lineSources{}
	}
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
	hasName := false
	for i, property := range card.Properties {
		switch strings.ToUpper(property.Name) {
		case "FN":
			if c.DisplayName == "" {
				c.DisplayName = text(property)
			} else {
				dropped = append(dropped, "FN")
			}
		case "N":
			// Graph holds one name; the first one that is not a pronunciation.
			if len(property.ParametersNamed("PHONETIC")) > 0 {
				continue
			}
			if hasName {
				dropped = append(dropped, "N")
				continue
			}
			hasName = true
			n := parts(property, 5)
			c.Surname, c.GivenName, c.MiddleName, c.Title, c.Generation = n[0], n[1], n[2], n[3], n[4]
		case "NICKNAME":
			if c.NickName == "" {
				c.NickName = text(property)
			} else {
				dropped = append(dropped, "NICKNAME")
			}
		case "CATEGORIES":
			values, err := vcard.SplitTextList(property.RawValue)
			if err != nil {
				values = []string{text(property)}
			}
			for _, value := range values {
				if value = strings.TrimSpace(value); value != "" && !slices.Contains(c.Categories, value) {
					c.Categories = append(c.Categories, value)
				}
			}
		case "EMAIL":
			if address := withoutScheme(text(property), "mailto:"); address != "" {
				c.EmailAddresses = append(c.EmailAddresses, emailAddress{Address: address})
				s.emails = append(s.emails, i)
			}
		case "TEL":
			number := withoutScheme(text(property), "tel:")
			if number == "" {
				continue
			}
			switch kinds := typeValues(property); {
			case noPhoneField(property):
			case slices.Contains(kinds, "cell") && c.MobilePhone == "":
				c.MobilePhone = number
				s.held = append(s.held, i)
			case slices.Contains(kinds, "home"):
				c.HomePhones = append(c.HomePhones, number)
				s.home = append(s.home, i)
			case slices.Contains(kinds, "cell") || slices.Contains(kinds, "work"):
				c.BusinessPhones = append(c.BusinessPhones, number)
				s.business = append(s.business, i)
			default:
				c.untypedPhones = append(c.untypedPhones, number)
				s.untyped = append(s.untyped, i)
			}
		// Graph holds one of each; the first line wins, as for FN, so a saved
		// line appended after an Outlook edit never takes over.
		case "ORG":
			if c.CompanyName+c.Department == "" {
				c.CompanyName, c.Department = organization(property.RawValue)
			} else {
				dropped = append(dropped, "ORG")
			}
		case "TITLE":
			if c.JobTitle == "" {
				c.JobTitle = text(property)
			} else {
				dropped = append(dropped, "TITLE")
			}
		case "NOTE":
			if c.PersonalNotes == "" {
				c.PersonalNotes = text(property)
			} else {
				dropped = append(dropped, "NOTE")
			}
		case "BDAY":
			if c.Birthday == nil && fullDate(property) {
				date, _ := vcard.ParsePartialDate(strings.TrimSpace(property.RawValue))
				// Midday UTC, as Outlook writes it, is the same day in every
				// time zone that Outlook shows it in.
				birthday := fmt.Sprintf("%04d-%02d-%02dT11:59:00Z", *date.Year, *date.Month, *date.Day)
				c.Birthday = &birthday
			} else if fullDate(property) {
				dropped = append(dropped, "BDAY")
			}
		case "ADR":
			adr := parts(property, 7)
			address := physicalAddress{Street: adr[2], City: adr[3], State: adr[4], PostalCode: adr[5], CountryOrRegion: adr[6]}
			// A PO box or extended address alone sends nothing, so its line stays overflow.
			if address.empty() {
				continue
			}
			kinds := typeValues(property)
			switch {
			case slices.Contains(kinds, "home") && c.HomeAddress.empty():
				c.HomeAddress = address
				s.held = append(s.held, i)
			case slices.Contains(kinds, "work") && c.BusinessAddress.empty():
				c.BusinessAddress = address
				s.held = append(s.held, i)
			case c.OtherAddress.empty():
				c.OtherAddress = address
				s.held = append(s.held, i)
			}
		}
	}
	return c, dropped
}

// fullDate reports whether a BDAY has a year, month and day, the only form
// that Graph can hold.
func fullDate(property vcard.Property) bool {
	date, err := vcard.ParsePartialDate(strings.TrimSpace(property.RawValue))
	return err == nil && date.Year != nil && date.Month != nil && date.Day != nil
}
