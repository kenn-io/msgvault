#!/usr/bin/env python3
from __future__ import annotations

import html.parser
import pathlib
import re
import sys
import urllib.parse

ROOT = pathlib.Path(__file__).resolve().parents[1]
SITE = ROOT / "site"

ROUTES = [
    "/",
]

REQUIRED_SITEMAP_URLS = [
    "https://msgvault.io/",
]

STATIC_ASSETS = [
    "favicon-192.png",
    "favicon-512.png",
    "favicon.svg",
    "how-it-works.svg",
    "oauth-multi-account.svg",
    "og-image.png",
    "og-image.svg",
]

GENERATED_ASSETS = [
    "concepts/account-collection-concept.png",
    "concepts/deduplication-concept.png",
    "concepts/oauth-multi-account-concept.png",
    "concepts/safety-ladder-concept.png",
    "concepts/survivor-selection-concept.png",
    "list-senders.svg",
    "stats.svg",
    "tui-all-messages.svg",
    "tui-deletion.svg",
    "tui-domains.svg",
    "tui-drilldown.svg",
    "tui-filter-modal.svg",
    "tui-labels.svg",
    "tui-message-detail.svg",
    "tui-search-drilldown.svg",
    "tui-search-sender.svg",
    "tui-search-subject.svg",
    "tui-selection.svg",
    "tui-senders.svg",
    "tui-subgroup-recipients.svg",
    "tui-subgroup-time.svg",
    "tui-thread.svg",
    "tui-time-daily.svg",
    "tui-time-monthly.svg",
    "tui-time-yearly.svg",
    "tui-time.svg",
]

FORBIDDEN_PATTERNS = [
    "virtual:starlight",
    "@astrojs/starlight",
    "<Tabs",
    "<TabItem",
    "<Card",
    "<CardGrid",
    "<Screenshot",
    "<Aside",
    "set:html",
]

ALLOWED_MISSING_LOCAL_PATHS = {
    "/install.sh",
    "/install.ps1",
}

FETCHED_LINK_RELS = {
    "apple-touch-icon",
    "apple-touch-startup-image",
    "icon",
    "manifest",
    "mask-icon",
    "modulepreload",
    "prefetch",
    "preload",
    "prerender",
    "stylesheet",
}

CSS_URL_RE = re.compile(
    r"url\(\s*(?:\"([^\"]*)\"|'([^']*)'|([^)]*?))\s*\)", re.IGNORECASE
)


def fail(message: str) -> None:
    print(f"FAIL: {message}", file=sys.stderr)
    raise SystemExit(1)


def route_to_file(route: str) -> pathlib.Path:
    if route == "/":
        return SITE / "index.html"
    return SITE / route.strip("/") / "index.html"


def is_local_file_path(path: str) -> bool:
    return pathlib.PurePosixPath(path).suffix != ""


def rel_tokens(value: str) -> set[str]:
    return {token.lower() for token in value.split()}


def is_fetched_link_resource(attrs: dict[str, str]) -> bool:
    return bool(rel_tokens(attrs.get("rel", "")) & FETCHED_LINK_RELS)


def srcset_urls(value: str) -> list[str]:
    urls: list[str] = []
    index = 0
    while index < len(value):
        while index < len(value) and value[index] in " \t\r\n,":
            index += 1
        if index >= len(value):
            break

        start = index
        while index < len(value) and not value[index].isspace() and value[index] != ",":
            index += 1
        url = value[start:index]
        if url:
            urls.append(url)

        while index < len(value) and value[index] != ",":
            index += 1
        if index < len(value):
            index += 1

    return urls


class LinkParser(html.parser.HTMLParser):
    def __init__(self) -> None:
        super().__init__()
        self.ids: set[str] = set()
        self.links: list[str] = []
        self.assets: list[str] = []
        self.style_attrs: list[str] = []
        self.style_blocks: list[str] = []
        self._in_style = False

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        attr = {key: value or "" for key, value in attrs}
        if "id" in attr:
            self.ids.add(attr["id"])
        if tag == "a" and "href" in attr:
            self.links.append(attr["href"])
        if tag in {"img", "script", "source"} and "src" in attr:
            self.assets.append(attr["src"])
        if tag in {"img", "source"} and "srcset" in attr:
            self.assets.extend(srcset_urls(attr["srcset"]))
        if tag == "video" and "poster" in attr:
            self.assets.append(attr["poster"])
        if tag == "link" and "href" in attr and is_fetched_link_resource(attr):
            self.assets.append(attr["href"])
        if "style" in attr:
            self.style_attrs.append(attr["style"])
        if tag == "style":
            self._in_style = True

    def handle_data(self, data: str) -> None:
        if self._in_style:
            self.style_blocks.append(data)

    def handle_endtag(self, tag: str) -> None:
        if tag == "style":
            self._in_style = False


def parse_html(path: pathlib.Path) -> LinkParser:
    parser = LinkParser()
    parser.feed(path.read_text(encoding="utf-8", errors="ignore"))
    return parser


def require_site_child(target: pathlib.Path, reference: str, current: pathlib.Path) -> pathlib.Path:
    resolved = target.resolve()
    site = SITE.resolve()
    if not resolved.is_relative_to(site):
        fail(f"local reference escapes site output: {reference} in {current}")
    return resolved


def target_file(current: pathlib.Path, href: str) -> pathlib.Path | None:
    parsed = urllib.parse.urlparse(href)
    if parsed.scheme or parsed.netloc or href.startswith("data:"):
        return None
    if parsed.path in ALLOWED_MISSING_LOCAL_PATHS:
        return None

    decoded_path = urllib.parse.unquote(parsed.path)
    if decoded_path.startswith("/"):
        if is_local_file_path(decoded_path):
            return require_site_child(SITE / decoded_path.lstrip("/"), href, current)
        route = decoded_path if decoded_path.endswith("/") else decoded_path + "/"
        return require_site_child(route_to_file(route), href, current)

    base = current.parent
    path = decoded_path or current.name
    resolved = (base / path).resolve()
    require_site_child(resolved, href, current)
    if resolved.is_dir():
        return require_site_child(resolved / "index.html", href, current)
    if resolved.suffix:
        return resolved
    return require_site_child(resolved / "index.html", href, current)


def check_local_asset(current: pathlib.Path, asset: str) -> pathlib.Path | None:
    parsed = urllib.parse.urlparse(asset)
    if parsed.scheme or parsed.netloc or asset.startswith("data:"):
        return None
    decoded_path = urllib.parse.unquote(parsed.path)
    if not decoded_path:
        return None
    if decoded_path.startswith("/"):
        target = SITE / decoded_path.lstrip("/")
    else:
        target = current.parent / decoded_path
    target = require_site_child(target, asset, current)
    if not target.is_file():
        fail(f"missing asset {asset} referenced by {current}")
    return target


def css_url_refs(text: str) -> list[str]:
    refs: list[str] = []
    for match in CSS_URL_RE.finditer(text):
        ref = next((group for group in match.groups() if group is not None), "")
        ref = ref.strip()
        if ref:
            refs.append(ref)
    return refs


def fragment_id(fragment: str) -> str:
    return urllib.parse.unquote(fragment)


def check_expected_asset_files() -> None:
    for asset in STATIC_ASSETS:
        path = SITE / "assets" / "static" / asset
        if not path.is_file():
            fail(f"missing built static asset {path.relative_to(SITE)}")
    for asset in GENERATED_ASSETS:
        path = SITE / "assets" / "generated" / asset
        if not path.is_file():
            fail(f"missing built generated asset {path.relative_to(SITE)}")


def main() -> None:
    if not SITE.exists():
        fail("site directory does not exist. Run the Zensical build first.")

    for route in ROUTES:
        path = route_to_file(route)
        if not path.exists():
            fail(f"missing route {route}: {path}")

    if not (SITE / "404.html").exists():
        fail("missing 404.html")
    if not (SITE / "sitemap.xml").exists():
        fail("missing sitemap.xml")
    sitemap_text = (SITE / "sitemap.xml").read_text(encoding="utf-8", errors="ignore")
    for url in REQUIRED_SITEMAP_URLS:
        if f"<loc>{url}</loc>" not in sitemap_text:
            fail(f"missing sitemap URL {url}")

    check_expected_asset_files()

    html_files = list(SITE.rglob("*.html"))
    all_text = "\n".join(
        path.read_text(encoding="utf-8", errors="ignore") for path in html_files
    )
    for pattern in FORBIDDEN_PATTERNS:
        if pattern in all_text:
            fail(f"forbidden generated marker found: {pattern}")

    parsed_by_file = {path.resolve(): parse_html(path) for path in html_files}
    for current, parser in parsed_by_file.items():
        for href in parser.links:
            parsed = urllib.parse.urlparse(href)
            if href.startswith("#"):
                fragment = fragment_id(parsed.fragment)
                if fragment and fragment not in parser.ids:
                    fail(f"missing local fragment {href} in {current}")
                continue
            target = target_file(current, href)
            if target is None:
                continue
            if target.suffix == ".html":
                if parsed.fragment:
                    target_parser = parsed_by_file.get(target.resolve())
                    if target_parser is None:
                        fail(f"missing linked page for fragment {href} in {current}")
                    if fragment_id(parsed.fragment) not in target_parser.ids:
                        fail(f"missing fragment {href} in {target}")
                elif not target.exists():
                    fail(f"missing internal page {href} in {current}")
            elif not target.exists():
                fail(f"missing linked file {href} in {current}")

        for asset in parser.assets:
            check_local_asset(current, asset)
        for css_text in parser.style_attrs + parser.style_blocks:
            for asset in css_url_refs(css_text):
                check_local_asset(current, asset)

    print("built site checks passed")


if __name__ == "__main__":
    main()
