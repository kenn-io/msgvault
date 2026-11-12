// Package messagebody provides shared fixtures for plain-text message readers.
package messagebody

import "strings"

const ContactForm = "From: sender@example.com\nDate: today\nSubject: Contact form\n\nPlease call me back\n"

// ReadableCases preserves authored text while removing quotes and signatures.
var ReadableCases = []struct{ Name, Body, Want string }{
	{"dated authored fragment", "On May 1, 2026 we agreed to ship\nAlice <alice@example.com> wrote:\n> old", "On May 1, 2026 we agreed to ship\nAlice <alice@example.com> wrote:"},
	{"complete URL", "https://example.com/doc?q=" + strings.Repeat("x", 200) + "#section", "https://example.com/doc?q=" + strings.Repeat("x", 200) + "#section"},
	{"complete URL before quotes", "https://example.com/doc?q=" + strings.Repeat("x", 200) + "#section\n> Old reply", "https://example.com/doc?q=" + strings.Repeat("x", 200) + "#section"},
	{"quote lines", "Current reply\n> old reply\n\nMore current text", "Current reply\n\nMore current text"},
	{"reply history", "Current reply\n\nOn Tue, Oct 1, 2024, Example Sender wrote:\n> Old reply\n> old signature", "Current reply"},
	{"unquoted history keeps its header", "Current reply\nOn Tue, Oct 1, 2024, Example Sender wrote:\nUnmarked text", "Current reply\nOn Tue, Oct 1, 2024, Example Sender wrote:\nUnmarked text"},
	{"dated prose without quotes", "On May 1, 2026 we wrote:\nRelease notes", "On May 1, 2026 we wrote:\nRelease notes"},
	{"wrapped bottom posted", "On Tue, Oct 1, 2024,\nExample Sender <sender@example.com> wrote:\n> Old reply\n\nCurrent reply", "On Tue, Oct 1, 2024,\nExample Sender <sender@example.com> wrote:\n\nCurrent reply"},
	{"bottom posted", "On Tue, Oct 1, 2024, Example Sender wrote:\n> Old reply\n\nCurrent reply", "Current reply"},
	{"inline reply", "First answer\nOn Tue, Oct 1, 2024, Example Sender wrote:\n> Old reply\nSecond answer\n> Another old line\nThird answer", "First answer\nSecond answer\nThird answer"},
	{"signature", "Current reply\n-- \nExample Sender\nhttps://example.com", "Current reply"},
	{"ordinary prose", "On this project we wrote:\nA useful paragraph", "On this project we wrote:\nA useful paragraph"},
	{"weekday prose", "On Monday we wrote:\nA useful paragraph", "On Monday we wrote:\nA useful paragraph"},
	{"month prefix prose", "On marketing we wrote:\nA useful paragraph", "On marketing we wrote:\nA useful paragraph"},
	{"prose before header", "Thanks.\nOn Monday we agreed to ship.\nOn Mon, Jan 5, 2026, Example Sender <sender@example.com> wrote:\n> old", "Thanks.\nOn Monday we agreed to ship."},
	{"dated prose before lowercase header", "On May 1, 2026 we agreed to ship.\non Tue, May 5, 2026, Sender wrote:\n> old reply", "On May 1, 2026 we agreed to ship."},
	{"bare dashes in prose", "Agenda\n--\nItem one", "Agenda\n--\nItem one"},
	{"dated sentence above a sender line", "On Monday at 10:00 we shipped the fix.\nAlice wrote:\n> q", "On Monday at 10:00 we shipped the fix.\nAlice wrote:"},
	{"address in prose above speaker", "On Jan 5, 2026 the team met with Carol <carol@example.com>.\nShe wrote:\n> excerpt\nMy reply below the excerpt.", "On Jan 5, 2026 the team met with Carol <carol@example.com>.\nShe wrote:\nMy reply below the excerpt."},
	{"address wrapped inside brackets", "Reply\nOn Tue, Oct 1, 2024 at 10:00 AM John Doe <\njohn@example.com> wrote:\n> old", "Reply\nOn Tue, Oct 1, 2024 at 10:00 AM John Doe <\njohn@example.com> wrote:"},
	{"gmail wrapped header", "Reply\nOn Tue, Oct 1, 2024 at 10:00 AM Example Sender <sender@example.com>\nwrote:\n> old", "Reply\nOn Tue, Oct 1, 2024 at 10:00 AM Example Sender <sender@example.com>\nwrote:"},
	{"single dash list in prose", "Sent from my laptop yesterday.\n- ship v2", "Sent from my laptop yesterday.\n- ship v2"},
	{"single dash list in prose before quotes", "Sent from my laptop yesterday.\n- ship v2\n> Old reply", "Sent from my laptop yesterday.\n- ship v2"},
	{"URL punctuation", "(See https://example.com/doc?id=" + strings.Repeat("x", 200) + "). Thanks", "(See https://example.com/doc?id=" + strings.Repeat("x", 200) + "). Thanks"},
	{"URL punctuation before quotes", "(See https://example.com/doc?id=" + strings.Repeat("x", 200) + "). Thanks\n> Old reply", "(See https://example.com/doc?id=" + strings.Repeat("x", 200) + "). Thanks"},
}
