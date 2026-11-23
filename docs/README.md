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
  is not necessarily in the latest release. The [changelog](https://msgvault.io/docs/changelog/) owns
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

For a command that has not shipped, put `Not in the latest release yet` before
its README block. For installer output, print `Not in the latest release yet`
immediately before the command and put `# Not in the latest release yet` before
the output in the source. See the
[release-commands guide](https://github.com/kenn-io/actions/tree/main/release-commands#mark-unreleased-commands)
for details.

## Add a release note

Add each note as its own file, `docs/changelog.d/<topic>.md`, instead of
editing `## Unreleased` in `docs/changelog.md`. Choose a `<topic>` that no
other note uses. Start with one section heading, then the bullets:

```markdown
### New features

- Describe the change for the reader.
```

The section must be `### Upgrade notes`, `### New features`,
`### Improvements`, or `### Bug fixes`. The published changelog lists the notes
under **Unreleased**, sorted by filename. Dot-prefixed files such as
`.draft.md` stay out of the published changelog.

If an open PR still edits `docs/changelog.md`, move its note into a fragment
when you rebase it.

To cut a release, a maintainer runs
`make changelog-release RELEASE_VERSION=0.27.0 RELEASE_DATE=2026-11-22`. Use
`X.Y.Z` for `RELEASE_VERSION` and `YYYY-MM-DD` for `RELEASE_DATE`. It needs
full Git history. It moves the notes under the version heading, keeping
the section headings, and deletes the files. Within each section, notes follow
the order each filename was added to `main`.

After `make changelog-release`, add the release summary and links, and update
`last_edited` as usual.

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

Every public page has a Markdown companion. The build copies documentation
sources to matching paths such as `/docs/setup.md` and `/docs/usage/searching.md`;
the documentation index is `/docs/index.md`. HTML pages link to their Markdown
companions with `rel="alternate"`. Keep `website/llms.txt` linked to every public
Markdown page. `make docs-check` verifies this coverage against the built site.

Deploy with `make docs-deploy-staging` for a Vercel preview or
`make docs-deploy` for production. Both targets pull the matching Vercel
environment, build locally with access to `docs/` and `website/`, and upload
the built site. Running `vercel` directly from `docs/` uploads only that
directory, so the remote build cannot find the sibling `website/` sources.

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

`docs-assets` holds curated images: the favicon, social card, and hand-drawn
diagrams. `docs-generated-assets` holds everything a script produces: the TUI
and CLI SVGs, the concept images, and the Web UI captures under `web/`.

`make docs-screenshots DOCS_SCREENSHOTS_OUTPUT=DIR` builds the complete
`docs-generated-assets` tree with only Bash, Git, and Docker on the host; the
automated screenshot runner uses this entry point. Its Web UI step imports the
pinned fixture into a temporary archive, builds and starts its own daemon, and
captures Messages and People at 1920 × 1080 with Playwright's Linux Chromium.
`make docs-web-screenshots` runs that step alone with the host toolchain; set
`MSGVAULT_DOCS_SCREENSHOT_OUTPUT_DIR` to keep captures outside the hydrated
asset directory while reviewing them.

Inspect every changed image before updating its branch. Keep the fixture's
provenance and review record on `docs-fixtures` unchanged unless the fixture
itself changes.
