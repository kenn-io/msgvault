# Legacy Discord CSV fixture

Derived from cobertos/base-data-manager, revision
`ab0a22641423662d3ba0ebdbe0c76a2e2eca7ad7`:

- `test/fixtures/discord-json-2021-01/messages/333333333333333333/messages.csv`
- `test/fixtures/discord-json-2021-01/messages/333333333333333333/channel.json`

Source: https://gitdab.com/cobertos/base-data-manager/src/commit/ab0a22641423662d3ba0ebdbe0c76a2e2eca7ad7/test/fixtures/discord-json-2021-01.md

The upstream author manually scrubbed this real legacy export. The two
files are retained verbatim. Every retained field was reviewed: placeholder
IDs, generic channel/guild names, synthetic message text, scrubbed mentions,
and placeholder URLs contain no personal identities. `account/user.json`
is synthetic and retains only a synthetic account ID. JSON transcripts in
package tests are entirely synthetic, not real JSON exports.

Upstream package.json declares ISC. Attribution: cobertos and contributors.

ISC License

Copyright (c) cobertos and contributors

Permission to use, copy, modify, and/or distribute this software for any
purpose with or without fee is hereby granted, provided that the above
copyright notice and this permission notice appear in all copies.

THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN ACTION
OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF OR IN
CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.

Reviewed fixture SHA-256 digests:

```text
d02a4f247a093b961969792bcc1863b154e119afec1dca2a8d1edac6ccda799e  account/user.json
2f97a350ce7efd81fec77fac0bd33e91c656e5e73494864792a869d9ee641c18  messages/333333333333333333/channel.json
da27c0bb4b8c0e6af5963edad8c36da811926ef216632ee0622fc15e9c6e49f5  messages/333333333333333333/messages.csv
```
