# Original fictional WhatsApp TXT fixtures

All names, chat text, timestamps and media bytes here were written for msgvault.
No third-party fixture, real conversation, identifier or attachment was copied.
`seed-note.txt` is original synthetic media. These files use the repository's
license; the references below supply structural evidence, not fixture content.

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
reference parser's guessing. `android-dmy.txt` uses LF, minute precision and a
file-attached suffix. `ios-mdy.txt` uses UTF-8 BOM, CRLF, a leading LRM, seconds,
English AM, a narrow no-break space and the LRM-prefixed `image omitted` marker.
Both include multiline text with a blank line, a notice with a colon, supplied media and an omitted-media marker.

Supported headers are `date, time - payload` and `[date, time] payload`.
Numeric dates use `/`, `.` or `-` consistently, explicit dmy/mdy/ymd ordering,
and two/four-digit years (00–68 means 2000–2068, 69–99 means 1969–1999).
Clocks use `H:mm`/`HH:mm`, optional `:ss`, and optional English AM/PM or a.m./p.m.
A normal, no-break or narrow no-break space may precede AM/PM. Initial LRM/RLM
and directional embedding marks are ignored only for recognizing a header;
raw records retain them. Attachment references are whole-line `<attached: NAME>`
(with optional spaces inside brackets) or `NAME (file attached)`;
`<Media omitted>` and the exact LRM-prefixed `image omitted` line count omitted
media. Other dialects are not promised.

Recognized English notices retain their entire payload without an asserted author.
Other colon payloads keep their exact prefix as an unverified scoped label.
Header-shaped lines inside a message cannot be distinguished from real records.
The unit tests add original fictional cases for time interpretation, invalid
input, bounded parsing, repeated and overlapping windows and media containment.

The iPhone U+200E `image omitted` structure was also checked against the
MIT-licensed [nicolasfeoli/whatsapp-chat-analyzer](https://github.com/nicolasfeoli/whatsapp-chat-analyzer)
at `a9215e94c94db372ea750f87a655537996e77fcb`:

- [parse-chat tests](https://github.com/nicolasfeoli/whatsapp-chat-analyzer/blob/a9215e94c94db372ea750f87a655537996e77fcb/tests/core/parsing/parse-chat.test.ts)
- [README](https://github.com/nicolasfeoli/whatsapp-chat-analyzer/blob/a9215e94c94db372ea750f87a655537996e77fcb/README.md), which identifies its example chats as invented
- [MIT license](https://github.com/nicolasfeoli/whatsapp-chat-analyzer/blob/a9215e94c94db372ea750f87a655537996e77fcb/LICENSE)

Only the structural marker informed our original fictional fixture. No chat
content or fixture was copied. No other iPhone omitted-media forms are claimed.
