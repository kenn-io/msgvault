# MCP Vector Chunk Matches Fixes Design

## Goal

Make semantic MCP search return useful chunk matches through the production
daemon path, while keeping the public tool contract honest about score
filtering, indexed content, and raw-body navigation.

## Current Problems

The in-process MCP handler can score chunks, but the production `msgvault mcp`
command delegates semantic search to the daemon. Its request and response types
carry only ranked message hits, so `matches` are absent and `min_score` is
ignored in normal use.

Stored chunk offsets refer to preprocessed subject-plus-body text. Preprocessing
can delete or rewrite raw body content, so subtracting the subject prefix does
not produce a reliable raw-body offset. The current response nevertheless
labels that value as suitable for `get_message center_at`.

Tool descriptions and user documentation also retain several contracts that no
longer match implementation: they direct vector callers to a keyword-only tool,
describe semantic search as body-only even though the index includes subjects,
overstate what `min_score` filters, omit `conversation_id`, and incorrectly
describe conditional tool registration.

## Design

### Production data flow

Chunk scoring will happen in the daemon/API process, where the hybrid engine,
vector backend, active generation, and message bodies already coexist. The
hybrid-search HTTP response will gain a bounded list of scored chunk matches per
message. The daemon client and MCP adapter will preserve those matches instead
of asking the MCP process to open a vector database or call the embedder again.

The MCP in-process path will use the same response-level match shape and helper
semantics. This keeps local/library use and production daemon use consistent.
Failures to enrich an individual result will remain best-effort, matching the
existing search hydration behavior; they must not fail the entire ranked search.

### `min_score`

`min_score` is a chunk-excerpt filter, not a message-ranking threshold. It will
be forwarded to the daemon and applied before chunk matches are serialized.
Ranked messages, pagination, `returned`, and `has_more` remain properties of the
message search and are not changed by excerpt filtering. Tool descriptions and
documentation will state this explicitly and will no longer claim that raising
the value prunes result messages.

### Match locations

Keyword matches continue to return required raw-body `char_offset` and `line`
fields.

Vector matches always return `snippet` and `score`. Their `char_offset` and
`line` fields become optional. The implementation may emit them only when the
preprocessed chunk can be located exactly and unambiguously in the raw body.
Otherwise the fields are omitted. This prevents incorrect `center_at`
navigation without requiring an embedding-schema migration or index rebuild.

Subject-containing chunks are valid semantic matches because the existing
embedding corpus is built from preprocessed subject plus body. Documentation
will describe that scope directly. A subject-only chunk therefore has no raw
body offset.

### Public contract cleanup

MCP tool descriptions and `docs/usage/chat.md` will:

- direct vector and hybrid searches to `semantic_search_messages`;
- describe semantic scope as preprocessed subject plus body;
- describe `min_score` as filtering returned chunk excerpts only;
- explain that vector locations can be absent;
- restore the supported `conversation_id` parameter for `list_messages`; and
- state that the semantic tool is always registered but advertises full vector
  parameters only when vector search is configured.

The existing testify-lint failure will use `assert.NotContains`.

## Testing

Regression coverage will exercise production behavior across component
boundaries:

- API hybrid search returns bounded scored chunk matches and honors
  `min_score` for excerpts.
- The generated/daemon client conversion preserves match fields.
- The MCP daemon adapter returns those matches to tool callers.
- In-process MCP search uses identical match semantics.
- Preprocessing that removes leading content does not produce a false raw-body
  offset; exactly locatable chunks still expose valid offsets.
- Tool-schema and documentation contract tests use the corrected tool names and
  descriptions.
- `make test`, `make lint-ci`, `go vet ./...`, and diff checks pass with the
  required `fts5 sqlite_vec` tags where applicable.

Tests and any branch binaries will use isolated temporary state and will not
start branch code against the live msgvault daemon or `~/.msgvault`.

## Rebase

After the fixes are committed and verified, the branch will be rebased onto the
latest `origin/main`. Conflicts will be resolved by preserving both current main
behavior and the contracts above, followed by a full verification rerun.
