#!/usr/bin/env python3
"""Make the published website pages paint in their own fonts from the first frame.

The website ships as static files with no bundler, so the build does what a
bundler would: it embeds every font in the render-blocking stylesheet as a data
URL, and inlines the first-paint hold script directly after each page's charset.
"""

from __future__ import annotations

import argparse
import base64
import pathlib
import re

FONT_URL = re.compile(r'url\("(/fonts/[^"]+\.woff2)"\)')
CHARSET = '<meta charset="utf-8">'
STYLESHEET = pathlib.Path("styles/site.css")
HOLD_SCRIPT = pathlib.Path("hold-first-paint.js")


def embed_fonts(stylesheet: pathlib.Path, website: pathlib.Path) -> None:
    def data_url(match: re.Match[str]) -> str:
        font = website / match.group(1).removeprefix("/")
        encoded = base64.b64encode(font.read_bytes()).decode("ascii")
        return f'url("data:font/woff2;base64,{encoded}")'

    embedded, count = FONT_URL.subn(data_url, stylesheet.read_text(encoding="utf-8"))
    if count == 0:
        raise SystemExit(f"{stylesheet}: no /fonts/*.woff2 url() to embed")
    stylesheet.write_text(embedded, encoding="utf-8")


def inline_hold_script(page: pathlib.Path, script: str) -> None:
    html = page.read_text(encoding="utf-8")
    if html.count(CHARSET) != 1:
        raise SystemExit(f"{page}: expected exactly one {CHARSET}")
    page.write_text(
        html.replace(CHARSET, f"{CHARSET}\n    <script>\n{script}    </script>", 1),
        encoding="utf-8",
    )


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--website", type=pathlib.Path, required=True, help="website/ sources")
    parser.add_argument("--site", type=pathlib.Path, required=True, help="built site root")
    args = parser.parse_args()
    website: pathlib.Path = args.website
    site: pathlib.Path = args.site

    embed_fonts(site / STYLESHEET, website)
    script = (website / HOLD_SCRIPT).read_text(encoding="utf-8")
    for source in sorted(website.rglob("*.html")):
        inline_hold_script(site / source.relative_to(website), script)


if __name__ == "__main__":
    main()
