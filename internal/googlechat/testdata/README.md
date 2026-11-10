# Google Chat Takeout fixtures

`redacted-public/` contains derivatives of a **real Google Chat Takeout export,
inferred from producer fingerprints; publisher provenance undocumented;
publisher hand-edits noted**. This is a scoped format observation, not a claim
that the publishers documented unmodified exports. The records date from 2024.
All names, addresses, identifiers, text, URLs and media bytes were replaced.
Provider keys, tokens, numeric annotation offsets and record relationships remain.

## Pinned sources

- [CivicTechAtlanta Space sample](https://github.com/CivicTechAtlanta/ieee-support/tree/477834ae0584d2b993b30452939a21ac37471572/IEEE-long-example),
  commit `477834ae0584d2b993b30452939a21ac37471572`. No license is stated.
  Only functional structure is retained; no expressive source content or media.
- [niimpy DM and user sample](https://github.com/digitraceslab/niimpy/tree/a2f4eb71fe3e14e835d8afd68c9b123e465a1374/niimpy/sampledata/Google/Takeout/Google%20Chat),
  commit `a2f4eb71fe3e14e835d8afd68c9b123e465a1374`. MIT grant and disclaimer
  are retained verbatim in [niimpy/LICENSE](redacted-public/niimpy/LICENSE).
  The pinned upstream license has no copyright line; none was invented.

| Source | File | Source SHA-256 |
| --- | --- | --- |
| civic | `messages.json` | `8edd25612e41f48c7f7ce4650155e6f8585134dafd303452e8d5ec2cf32068e2` |
| civic | `group_info.json` | `2218d1348bc41372b646d8bb97b56dd7724cb4ad400196d868d836bd53153ea7` |
| niimpy | `messages.json` | `ed0ed26712dfefe0b537d24d8307d3f1f28d150f4fd133a8cdbe9bc60cce0ae2` |
| niimpy | `group_info.json` | `fd7cf67965b4c3e875812020afdd675b952763443a6dc76a0c62cc8a12162665` |
| niimpy | `user_info.json` | `bee0bcfb73b82742182f8dacccaf6f27d8eb1d92ebc2af356631a31ef3ef7d47` |

## Redaction and retained evidence

| Value class | Replacement or retention |
| --- | --- |
| Names and email addresses | Consistent `Person N` and `personN@example.com` per source identity; publisher's email-domain typo mapped consistently |
| Group, topic, message and user IDs | Invented values of the same length and character class; top-level and reply relationships preserved |
| Dates | Shifted 370 days, weekdays recomputed; English template, hour width, U+202F and UTC retained |
| Text, quoted text, URL titles/snippets | Filler with the same character lengths; absent text stays absent |
| URLs | Reserved `https://example.com/` URLs |
| Filenames | Invented stems with original lengths/extensions; `File-` relationship retained |
| Media | Independently generated one-pixel images; no source media fetched |
| Provider structure | Every record, order, key, type, array length and optional key set retained |
| Provider tokens | `Human`, format types, font colors, membership states and emoji Unicode retained |

The Civic derivative has 84 messages, 2 replies, 3 absent text fields, 13
annotations, 6 quotations and 4 reaction entries. Annotation keys and offsets
remain as observed, including zero-length annotations on records without text.
There are 6 attachment references on 4 messages. Five synthetic files match
exactly; the JPEG filename retains the observed truncation (78 referenced
characters versus 51 on disk), so it is reported missing. Quoted attachment
metadata is retained raw and does not create another attachment slot.

Civic's publisher renamed its folder. `Groups/Space <id>` is inferred from the
DM layout and message IDs. The combined `Takeout/Google Chat/{Groups,Users}`
layout is inferred; no single real sample shows that entire tree together.

niimpy contributes 4 DM messages, group members and `user_info.json` membership
structure. Its publisher edited identities and produced a duplicate message ID
for two different records. This derivative assigns that record a distinct
invented message/topic ID together, preserving its top-level relationship.
The conflicting-ID pattern is covered separately by the synthetic
`TestImportGoogleChatConflictingDuplicateRecord` regression. The publisher's
explicitly invented `another_user_info.json` is excluded.

A retained vault-only redaction script verified source hashes, equal key paths,
types, array lengths, optional key sets and reply counts. Decoded string leaves
were checked against non-allowlisted source values; substantive strings were
also scanned as raw and JSON-escaped substrings. Incidental short substrings in
provider key names do not constitute retained content. All output addresses are
reserved. The script is outside this repository because it processes source
content. Output digests below make the reviewed artifacts reproducible.

| Derivative path (under `testdata/`) | SHA-256 |
| --- | --- |
| `redacted-public/civic/Groups/Space QKEMddQcZK8/messages.json` | `7d800a959dd31e39fd5a927e23505d78b97287331412b7c45a2f39b29b7303fa` |
| `redacted-public/civic/Groups/Space QKEMddQcZK8/group_info.json` | `e9695797e28914306575c442964ec317bec1682d58fb8d706d9e9be503b64d7f` |
| `redacted-public/civic/Groups/Space QKEMddQcZK8/File-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx1.png` | `ee945a190f8d9e500ff9a1a9feb6e678980e0c5f5d807eeea5b1e357195f7cb1` |
| `redacted-public/civic/Groups/Space QKEMddQcZK8/File-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx.jpg` | `27386ce4dfbda46cde1440ffb06e3dd0d2b44dbc7e27a9598976759e351bef3f` |
| `redacted-public/civic/Groups/Space QKEMddQcZK8/File-xxxxxxxxxxx3.png` | `ee945a190f8d9e500ff9a1a9feb6e678980e0c5f5d807eeea5b1e357195f7cb1` |
| `redacted-public/civic/Groups/Space QKEMddQcZK8/File-xxxxxxx4.bmp` | `1473ed058f811bf3e8a82c638b8b4b9f95bc2131a27374f010b16c48e973cd2d` |
| `redacted-public/civic/Groups/Space QKEMddQcZK8/File-xxxxxxxxxx5.bmp` | `1473ed058f811bf3e8a82c638b8b4b9f95bc2131a27374f010b16c48e973cd2d` |
| `redacted-public/civic/Groups/Space QKEMddQcZK8/File-xxxxxxx6.gif` | `ef1955ae757c8b966c83248350331bd3a30f658ced11f387f8ebf05ab3368629` |
| `redacted-public/niimpy/Groups/DM 7soV-5FKECQ/messages.json` | `0ff36aef2184ae9ee64a79ee97ade0a7cbe74dace73421b7a2648a09bd67a988` |
| `redacted-public/niimpy/Groups/DM 7soV-5FKECQ/group_info.json` | `b8b8e16de96a5b0a740187007e3a748c62162d5368e748279cdd012c57a86ffa` |
| `redacted-public/niimpy/Users/User 963762998917622426419/user_info.json` | `f2ac659d0b9c2de4c09de54e08bbf83c4af7ac864aa1e255a725356201234ed7` |

## Scope and synthetic edge cases

Real evidence covers personal-account, English-locale, UTC-suffixed 2024
variants with Human senders, a two-person DM, Space records, thread IDs and exact
`File-` references. It does not establish current exports, non-UTC/localized
dates, other sender types, larger group DMs, edit/deletion markers, Workspace or
externally owned spaces, or actual multipart splitting. Multipart behavior is
verified only with synthetic cases.

`takeout/` is wholly invented: names, addresses, IDs, text, quotations and media.
Its DM/user layout follows niimpy; Space optional metadata follows Civic.
`future`/`future_group`, unknown senders/dates, missing IDs and media are
synthetic edge cases, not asserted provider fields. Tests also generate
traversal, symlink, multipart, conflict, empty and sparse oversized files in
temporary directories. No test reads a default export, account or archive.
