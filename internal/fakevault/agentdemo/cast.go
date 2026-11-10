package agentdemo

import (
	"context"
	"fmt"
	"strings"
)

var castSources = []struct{ handle, typ, identifier, family string }{
	{"gmail", "gmail", "morgan@example.com", emailKind},
	{"whatsapp", "whatsapp", "+15550100100", "chat"},
	{"signal", "beeper", "signal", "chat"},
	{"imessage", "apple_messages", "local", "chat"},
	{slackSource, slackSource, "demo-workspace", "chat"},
	{"discord", "discord", "demo-guild", "chat"},
	{"meetings", "meeting_import", "demo-notes", "meetings"},
}
var castParticipants = []struct{ handle, name, typ, value string }{
	{"owner", "Morgan Lee", emailKind, "morgan@example.com"},
	{ownerPhoneHandle, "Morgan Lee", "phone", "+15550100100"},
	{"owner_slack", "Morgan Lee", slackSource, "U0OWNER"},
	{"sam", "Sam Rivera", emailKind, "sam.rivera@example.org"},
	{"sam_phone", "Sam Rivera", "phone", "+15550100101"},
	{"priya", "Priya Shah", emailKind, "priya@corp.example"},
	{"priya_slack", "Priya Shah", slackSource, "U0PRIYA"},
	{"jordan", "Jordan Kim", emailKind, "jordan@client.example"},
	{"taylor", "Taylor Brooks", emailKind, "taylor@partner.example"},
	{"casey", "Casey Nguyen", "phone", "+15550100105"},
	{"riley", "Riley Stone", emailKind, "riley@newco.example"},
	{"pat", "Pat Vendor", emailKind, "pat@vendor.example"},
	{"background", "Archive Updates", emailKind, "archive-updates@example.net"},
	{"support_vendor", "Vendor Support", emailKind, "support@vendor.example"},
	{"noreply", "Service Notifications", emailKind, "no-reply@service.example"},
	{"newsletter", "Weekly Digest", emailKind, "digest@news.example"},
	{"slackbot", "Deploy Bot", slackSource, "B0DEPLOY"},
	{"shortcode", "72345", "sms", "72345"},
	{"canary", "Canary Q. Person", emailKind, "canary-addr-7f3a@canary.example"},
	{"filler_one", "Jamie Example", "phone", "+15550100106"},
	{"filler_two", "Robin Example", "phone", "+15550100107"},
}
var castPeople = []struct {
	handle, name string
	participants []string
}{
	{"sam", "Sam Rivera", []string{"sam", "sam_phone"}},
	{"priya", "Priya Shah", []string{"priya", "priya_slack"}},
	{"jordan", "Jordan Kim", []string{"jordan"}},
	{"taylor", "Taylor Brooks", []string{"taylor"}},
	{"casey", "Casey Nguyen", []string{"casey"}},
}

func cast(ctx context.Context, b *builder) error {
	for _, s := range castSources {
		row, err := b.st.GetOrCreateSourceContext(ctx, s.typ, s.identifier)
		if err != nil {
			return err
		}
		b.m.Sources[s.handle] = Source{row.ID, s.typ, s.identifier, s.family}
	}
	for _, p := range castParticipants {
		var id int64
		var err error
		switch p.typ {
		case emailKind:
			_, domain, _ := strings.Cut(p.value, "@")
			id, err = b.st.EnsureParticipantContext(ctx, p.value, p.name, domain)
		case "phone":
			id, err = b.st.EnsureParticipantByPhone(ctx, p.value, p.name, "phone")
		default:
			id, err = b.st.EnsureParticipantByIdentifierContext(ctx, p.typ, p.value, p.name)
		}
		if err != nil {
			return fmt.Errorf("participant %s: %w", p.handle, err)
		}
		b.m.Participants[p.handle] = Participant{id, p.typ + ":" + p.value}
	}
	for _, p := range castPeople {
		ids := []int64{}
		identities := []string{}
		for _, h := range p.participants {
			ids = append(ids, b.participant(h))
			identities = append(identities, b.m.Participants[h].Identity)
		}
		for _, id := range ids[1:] {
			if _, err := b.st.LinkParticipants(ids[0], id); err != nil {
				return err
			}
		}
		person, _, err := b.st.CreatePersonFromParticipantWithDisplayNameContext(ctx, ids[0], &p.name)
		if err != nil {
			return err
		}
		b.m.Persons[p.handle] = Person{person.ID, ids, identities}
	}
	for _, s := range castSources {
		address := b.m.Owner.Email
		if s.family == "chat" {
			address = b.m.Owner.Phone
		}
		if s.handle == slackSource {
			address = "U0OWNER"
		}
		if err := b.st.AddAccountIdentityContext(ctx, b.source(s.handle), address, "profile"); err != nil {
			return err
		}
	}
	return nil
}
