# Documentation contributor guide

Help readers choose a workflow, complete it, and understand its limits. The
public site has three levels; keep their responsibilities distinct.

| Source | Reader question | Owns |
|---|---|---|
| `website/index.html` and `website/index.md` | Why use msgvault? | Product purpose and capabilities |
| `website/guide/index.html` and `website/guide.md` | How does an archive fit together? | The capture-to-ownership lifecycle |
| `docs/index.md` and `docs/zensical.toml` | Where do I go next? | Task routing and navigation |
| `docs/guides/` and `docs/usage/` | How do I do this? | Procedures, choices, and task-specific limits |
| CLI, configuration, and API references | What is the exact contract? | Flags, fields, defaults, authorization, and errors |
| `docs/architecture/` | How does the current system work? | Responsibilities, data flow, invariants, and backend limits |
| `docs/changelog.md` | What changed in a release? | Release history and upgrade implications |
| `docs/internal/` | Why was this decision made? | Engineering records, proposals, and historical plans |

## Write and maintain a page

- Start with the reader's outcome. Explain the smallest amount of context needed
  to take the next step, then put detailed contracts below it or link to them.
- Separate what works today, its limitations, and future work. A merged change
  is not necessarily in the latest release. The [changelog](changelog.md) owns
  release changes and upgrade notes; keep unreleased work separate from dated
  releases. Task guides own current usage, not a second release catalog.
- Update the owning section. Do not append a running implementation diary or
  copy whole command catalogs into README and indexes.
- Preserve command spelling, defaults, authorization checks, failure behavior,
  and any active exceptions. Check changes against current commands, tests,
  configuration types, and `api/openapi.yaml` rather than an old plan alone.
- Keep the product and lifecycle HTML pages and their Markdown companions
  consistent. Update `website/llms.txt` when adding a useful reader route.
- Keep useful existing anchors when reorganizing pages, or update all incoming
  links. Prefer relative `.md` links between docs; use `/docs/.../` in website
  HTML and `/docs/assets/static/` or `/docs/assets/generated/` for docs media.

## Living architecture and historical records

Architecture pages describe the system now. Use sections for purpose, current
capabilities, responsibilities, data flow, rules and failure behavior, and open
decisions only when the topic needs them. A diagram can explain a relationship;
a package inventory does not replace an explanation of who owns the data.

Keep proposals and approved but unbuilt work explicitly labeled. When a design
is implemented or superseded, link its current owning guide and mark the old
document as a historical record. Preserve rationale, recorded approvals, and
active exceptions, including the conditions for removing them. Do not infer
completion or approval from a plan's existence or checkboxes.

`docs/internal/` is excluded from the public site and normal user navigation.
Its [index](internal/README.md) routes maintainers to historical material; it
must not become a second implementation-status catalog. New work does not need
a mandatory template or a design document unless the problem warrants one.

## Build and inspect

From the repository root:

```bash
make docs-install
make docs-build
make docs-check
```

The Zensical project lives here. The build puts documentation under `/docs/`
and copies the static marketing site to `/`. `make docs-check` runs source
validation, builds that actual layout, and checks the output and redirects.
Use `make docs-serve` to inspect it at `http://127.0.0.1:8000`.

Check a representative rendered page after changing structure, navigation,
tables, diagrams, or HTML. Inspect links, headings, and mobile line wrapping.
For prose changes, run the existing docs checks; do not add tests that search
for the words you just wrote.

## Media

Curated and generated docs media live on the `docs-assets` and
`docs-generated-assets` orphan branches. Builds hydrate ignored
`docs/assets/static/` and `docs/assets/generated/` directories from those
branches. Do not commit hydrated files on the source branch.

Follow the [diagram guide](diagrams/README.md) and the scripts in
`docs/screenshots/` when changing media. The public Enron screenshot fixture
has a narrow provenance and privacy-review exception in [AGENTS.md](../AGENTS.md);
it does not authorize reuse in ordinary tests.
