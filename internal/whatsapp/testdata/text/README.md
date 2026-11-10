# Original fictional WhatsApp TXT fixtures

All names, chat text, timestamps and media bytes in the tests were written for msgvault.
No third-party fixture, real conversation, identifier or attachment was copied.
The references below supply structural evidence, not fixture content.

Official guidance describes per-chat TXT exports with optional media:

- [Android export guidance](https://faq.whatsapp.com/1180414079177245/?cms_platform=android)
- [iPhone export guidance](https://faq.whatsapp.com/1180414079177245/?cms_platform=iphone)

That guidance does not publish a normative byte grammar. The observed header,
multiline, senderless-notice, directional-mark, narrow-space and attachment
patterns were studied in the MIT-licensed
[Pustur/whatsapp-chat-parser](https://github.com/Pustur/whatsapp-chat-parser)
at commit `57484d5a53cacff39083237825f8cf2df7501129`:

- [parser tests](https://github.com/Pustur/whatsapp-chat-parser/blob/57484d5a53cacff39083237825f8cf2df7501129/tests/parser.test.ts)
- [date tests](https://github.com/Pustur/whatsapp-chat-parser/blob/57484d5a53cacff39083237825f8cf2df7501129/tests/date.test.ts)
- [time tests](https://github.com/Pustur/whatsapp-chat-parser/blob/57484d5a53cacff39083237825f8cf2df7501129/tests/time.test.ts)
- [MIT license](https://github.com/Pustur/whatsapp-chat-parser/blob/57484d5a53cacff39083237825f8cf2df7501129/LICENSE)

msgvault implements a smaller subset, with explicit date order instead of the
reference parser's guessing.

Supported headers are `date, time - payload` and `[date, time] payload`.
Numeric dates use `/`, `.` or `-` consistently, explicit dmy/mdy/ymd ordering,
and two/four-digit years (00–68 means 2000–2068, 69–99 means 1969–1999).
Clocks use `H:mm`/`HH:mm`, optional `:ss`, and optional English AM/PM or a.m./p.m.
A normal, no-break or narrow no-break space may precede AM/PM. Initial LRM/RLM
and directional embedding marks are ignored only for recognizing a header;
raw records retain them. Attachment references are whole-line `<attached: NAME>`
(with optional spaces inside brackets) or `NAME (file attached)`;
`<Media omitted>` and U+200E-marked English iPhone `… omitted` placeholders
count omitted media, including the optional marked English edited note.
Other dialects are not promised.

Recognized English notices retain their entire payload without an asserted author.
Other colon payloads keep their exact prefix as an unverified scoped label.
Supported headers inside message text start new records. Other lines beginning
with a bracketed prefix and text, a comma-bearing prefix and a spaced dash, or
a prefix and a spaced dash followed by a `sender:` label are rejected. Unicode
spaces and dashes count. Ordinary text such as `Meet at 10:01` stays in the
body; `[todo] buy seeds` is rejected. Unknown headers outside these envelopes
can remain continuation text. The unit tests cover time interpretation, invalid
input, bounded parsing, repeated and overlapping windows and media containment.

The U+200E-marked English iPhone `… omitted` placeholders and English group-creation and
subject-change notice structures were also checked against the
MIT-licensed [nicolasfeoli/whatsapp-chat-analyzer](https://github.com/nicolasfeoli/whatsapp-chat-analyzer)
at `a9215e94c94db372ea750f87a655537996e77fcb`:

- [group-notice tests](https://github.com/nicolasfeoli/whatsapp-chat-analyzer/blob/a9215e94c94db372ea750f87a655537996e77fcb/tests/core/parsing/group-notices.test.ts), covering creation and subject changes with quoted names
- [group-notice parser](https://github.com/nicolasfeoli/whatsapp-chat-analyzer/blob/a9215e94c94db372ea750f87a655537996e77fcb/src/core/parsing/group-notices.ts)
- [invented group events](https://github.com/nicolasfeoli/whatsapp-chat-analyzer/blob/a9215e94c94db372ea750f87a655537996e77fcb/tests/fixtures/group-events.ts)
- [message-classification source](https://github.com/nicolasfeoli/whatsapp-chat-analyzer/blob/a9215e94c94db372ea750f87a655537996e77fcb/src/core/parsing/message-classification.ts), lines 111–112 and 250–270: marked typed placeholders
- [message-classification tests](https://github.com/nicolasfeoli/whatsapp-chat-analyzer/blob/a9215e94c94db372ea750f87a655537996e77fcb/tests/core/parsing/message-classification.test.ts), lines 52–55 and 244–276: marked English placeholders, edited note and unmarked text
- [media-type tests](https://github.com/nicolasfeoli/whatsapp-chat-analyzer/blob/a9215e94c94db372ea750f87a655537996e77fcb/tests/core/analysis/media-type.test.ts), lines 8–16: English kinds
- [parse-chat tests](https://github.com/nicolasfeoli/whatsapp-chat-analyzer/blob/a9215e94c94db372ea750f87a655537996e77fcb/tests/core/parsing/parse-chat.test.ts)
- [README](https://github.com/nicolasfeoli/whatsapp-chat-analyzer/blob/a9215e94c94db372ea750f87a655537996e77fcb/README.md), which identifies its example chats as invented
- [MIT license](https://github.com/nicolasfeoli/whatsapp-chat-analyzer/blob/a9215e94c94db372ea750f87a655537996e77fcb/LICENSE)

Only these structures informed our original fictional fixtures and parser
cases. No chat content, parser code or fixture was copied. Non-English
omitted-media forms are not claimed.
