# Google OAuth screenshots

These published captures illustrate the Gmail onboarding guide without requiring
an authenticated Google Cloud session. Retrieved and visually reviewed on
2026-10-02. UI wording can change; the guide owns the current instructions.

## Sources and licenses

| Published file | Attribution and source | License | Changes |
| --- | --- | --- | --- |
| `client-created.png` | Google, [Vertex AI / Workspace codelab](https://codelabs.developers.google.com/vertexai-gws-agents?hl=en); [original image](https://codelabs.developers.google.com/static/vertexai-gws-agents/img/c1c9bc2f8c14dd6c.png) | [CC BY 4.0](https://creativecommons.org/licenses/by/4.0/), per the source page's content notice | Opaque redactions over project name, client ID, and client secret; PNG re-rendered without metadata |
| `unverified-app.png` | Google, [OAuth codelab](https://developers.google.com/health/codelabs/make-your-first-api-call); [original image](https://developers.google.com/static/health/codelabs/make-your-first-api-call/images/unverified.png) | [CC BY 4.0](https://creativecommons.org/licenses/by/4.0/), per the source page's content notice | None |

Google's pages link to its [site policies](https://developers.google.com/terms/site-policies).
No separate copyright exclusion was identified for these two instructional
screenshots. Attribution and modification notices also appear beneath each
image in the guide.

## Privacy review and reproducibility

The warning image contains no account, app, or credential identifiers. The client
creation image originally displayed credentials from the public codelab. The
published derivative hides the project name, entire client ID, and secret using
opaque masks; it retains the Desktop app selection, Download JSON button, and
one-time download warning. Its Internal organization banner is unchanged and
explained in the guide's caption.

The client image is 705 × 768 pixels. Redaction rectangles use `(x, y, width,
height)` in source pixels: project `(196, 0, 187, 44)`, client ID
`(376, 302, 275, 99)`, and secret `(376, 525, 272, 48)`. Values were covered before
rasterizing the published PNG. No hidden layers or source values remain in it.
The two final images were inspected visually and contain no personal addresses,
tokens, or unmasked credential values. Originals stay outside the repository.

| File | Original SHA-256 | Published SHA-256 |
| --- | --- | --- |
| `client-created.png` | `c2a18cf569ec3793b0452aec4f9919deb1804e0d2d6c6fad529859ee56dcbc83` | `75e7dc522f9a5ac64ca70f4b12fa63a025fc5816ad7188c55b0fddf815c7253c` |
| `unverified-app.png` | `aea3b2ba86de0eafd49f988e619fe40a9bc93c34fa83172f0b78736826961a7c` | `aea3b2ba86de0eafd49f988e619fe40a9bc93c34fa83172f0b78736826961a7c` |

Keep this source record in `docs/screenshots/google-oauth.md` and the image
credits in the guide captions. Hydrated images belong in the ignored
static-assets directory, not on the source branch.
