# Fictional Discord JSON package fixture

Authored independently for msgvault. All IDs, names, timestamps, messages,
and URLs are invented; URLs use the reserved `example.com` domain. No
third-party fixture bytes or personal export content were copied.

This fixture exercises the observed mock layout: `Account/user.json`,
`Messages/index.json`, and `Messages/c<channel-ID>/channel.json` alongside
`messages.json`. It includes a DM with string recipient IDs and a
`GUILD_TEXT` channel with nested guild metadata. Transcripts are arrays with
string `ID`, `Timestamp`, `Contents`, and `Attachments` fields. Attachment
strings contain whitespace-separated links; msgvault stores them without
fetching them.

Structural evidence, inspected on 2026-11-10:

- [Discord's official data package documentation](https://support.discord.com/hc/en-us/articles/360004957991-Your-Discord-Data-Package)
  documents JSON packages, channel folders and metadata, sent-message coverage,
  and message field meanings. No documentation prose or assets were copied.
- [BrannonSD/discord-data-export-explorer](https://github.com/BrannonSD/discord-data-export-explorer/tree/3326529c00800bad104059d8394d80c10bdae269),
  full revision `3326529c00800bad104059d8394d80c10bdae269`, publishes a
  [mock package fixture](https://github.com/BrannonSD/discord-data-export-explorer/tree/3326529c00800bad104059d8394d80c10bdae269/tests/fixtures/mock_package)
  under the [MIT license](https://github.com/BrannonSD/discord-data-export-explorer/blob/3326529c00800bad104059d8394d80c10bdae269/LICENSE)
  (copyright 2026 BrannonSD). Its
  [server](https://github.com/BrannonSD/discord-data-export-explorer/blob/3326529c00800bad104059d8394d80c10bdae269/server.js)
  consumes that layout, and its
  [integration tests](https://github.com/BrannonSD/discord-data-export-explorer/blob/3326529c00800bad104059d8394d80c10bdae269/tests/test_server.js)
  exercise the mock package. Only structural facts informed this fixture;
  upstream IDs, names, text, URLs, and file contents were not reused.

`TestPackageJSONFixture` imports this directory through the production
importer. The existing snapshot test covers mutations and replay separately.
This is synthetic data informed by official documentation and an observed
parser mock layout. It is not a Discord-produced export and does not verify
compatibility with every current producer variant. JSON import remains
experimental. The separate legacy CSV fixture retains its own provenance.
