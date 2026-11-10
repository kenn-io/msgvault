package agentdemo

import (
	"context"
	"fmt"
	"strings"
)

var castSources = []struct{ handle, typ, identifier, family string }{
	{"gmail", "gmail", "morgan@example.com", "email"},
	{"whatsapp", "whatsapp", "+15550100100", "chat"},
	{"signal", "beeper", "signal", "chat"},
	{"imessage", "apple_messages", "local", "chat"},
	{"slack", "slack", "demo-workspace", "chat"},
	{"discord", "discord", "demo-guild", "chat"},
	{"meetings", "meeting_import", "demo-notes", "meetings"},
}
var castParticipants = []struct{ handle, name, typ, value string }{
	{"owner", "Morgan Lee", "email", "morgan@example.com"},
	{"owner_phone", "Morgan Lee", "phone", "+15550100100"},
	{"owner_slack", "Morgan Lee", "slack", "U0OWNER"},
	{"sam", "Sam Rivera", "email", "sam.rivera@example.org"},
	{"sam_phone", "Sam Rivera", "phone", "+15550100101"},
	{"priya", "Priya Shah", "email", "priya@corp.example"},
	{"priya_slack", "Priya Shah", "slack", "U0PRIYA"},
	{"jordan", "Jordan Kim", "email", "jordan@client.example"},
	{"taylor", "Taylor Brooks", "email", "taylor@partner.example"},
	{"casey", "Casey Nguyen", "phone", "+15550100105"},
	{"riley", "Riley Stone", "email", "riley@newco.example"},
	{"pat", "Pat Vendor", "email", "pat@vendor.example"},
	{"background", "Archive Updates", "email", "archive-updates@example.net"},
	{"support_vendor", "Vendor Support", "email", "support@vendor.example"},
	{"noreply", "Service Notifications", "email", "no-reply@service.example"},
	{"newsletter", "Weekly Digest", "email", "digest@news.example"},
	{"slackbot", "Deploy Bot", "slack", "B0DEPLOY"},
	{"shortcode", "72345", "sms", "72345"},
	{"canary", "Canary Q. Person", "email", "canary-addr-7f3a@canary.example"},
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
		case "email":
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
		if s.handle == "slack" {
			address = "U0OWNER"
		}
		if err := b.st.AddAccountIdentityContext(ctx, b.source(s.handle), address, "profile"); err != nil {
			return err
		}
	}
	return nil
}
