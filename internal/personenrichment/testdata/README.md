---
last_edited: 2026-08-23
---

# Exa contract fixtures

These minimum synthetic fixtures freeze the documented request and response
shapes used by the Exa adapter. They were derived from the official Exa
[People Search reference](https://exa.ai/docs/reference/verticals/people-for-coding-agents)
and [Search reference](https://exa.ai/docs/reference/search), accessed
2026-08-22.

- `exa_people_success.json` covers a typed person entity and documented cost.
- `exa_deep_success.json` covers generated structured output with field-level
  grounding and citations.
- `exa_people_error.json` covers the documented request ID on an error body.

All names, identifiers, organizations, URLs, and content are synthetic. The
fixtures contain no real person and no credential.

# Sixtyfour contract fixtures

These minimum synthetic fixtures freeze the asynchronous start, poll, result,
and terminal-error shapes used by the Sixtyfour adapter. They were derived
only from the official Sixtyfour
[People Intelligence reference](https://docs.sixtyfour.ai/api-reference/endpoint/people-intelligence),
accessed 2026-08-22.

- `sixtyfour_start.json` covers the opaque task ID and uppercase start status.
- `sixtyfour_pending.json` covers a non-terminal poll response.
- `sixtyfour_complete.json` covers requested structured data, the documented
  global confidence score, deprecated empty findings, and a whole-US-cent
  charge.
- `sixtyfour_error.json` covers a lowercase terminal job status and safe error
  field.

All fixture names, identifiers, organizations, URLs, and values are synthetic.
The fixtures contain no real person and no credential.

The named official page and its OpenAPI response schema do not document a
returned identity envelope, provider person ID, canonical URL, freshness,
source/citation, or provider/model version fields. Fixtures and the strict wire
codec omit those fields rather than inventing trust metadata. Consequently, a
first-time result with no independently verified prior provider ID remains
identity-rejected and auditable; it cannot project automatically.
