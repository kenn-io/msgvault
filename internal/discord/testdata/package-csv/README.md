# Fictional legacy Discord CSV package fixture

Authored independently for msgvault. All IDs, names, dates, messages, and URLs
are invented. URLs use reserved `example.org` domains. No third-party fixture
bytes or personal export content were copied.

The five rows cover plain text, CSV quotes and commas, multiline Unicode text,
an attachment-only message, and a URL in message text. The explicit UTC offset
and fractional seconds exercise the importer's timestamp parser. The fixture
uses the supported lowercase `account/` and `messages/` directory names.

Structural evidence, inspected on 2026-11-10:

- [Discord's official data package documentation](https://support.discord.com/hc/en-us/articles/360004957991-Your-Discord-Data-Package)
  supplies current message field meanings and sent-message coverage. It
  describes JSON packages; it does not establish a current CSV producer.
- [BrannonSD/discord-data-export-explorer](https://github.com/BrannonSD/discord-data-export-explorer/tree/3326529c00800bad104059d8394d80c10bdae269),
  full revision `3326529c00800bad104059d8394d80c10bdae269`, publishes a
  [mock CSV transcript](https://github.com/BrannonSD/discord-data-export-explorer/blob/3326529c00800bad104059d8394d80c10bdae269/tests/fixtures/mock_package/Messages/c100000000000000002/messages.csv)
  with the `ID,Timestamp,Contents,Attachments` header and CSV quoting layout.
  The repository's [MIT license](https://github.com/BrannonSD/discord-data-export-explorer/blob/3326529c00800bad104059d8394d80c10bdae269/LICENSE)
  credits BrannonSD (2026). Only structural facts informed this fixture;
  upstream IDs, names, timestamps, messages, URLs, and file contents were
  not reused.

`TestPackageLegacyCSV` and the CLI end-to-end test import this package through
production code. This verifies msgvault's supported CSV parsing against
fictional input, not compatibility with a real Discord-produced export.
The separate JSON fixture also remains synthetic, and JSON import remains
experimental.
