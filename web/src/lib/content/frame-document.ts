import { prohibitedRemoteHost } from './url-safety';

export const ARCHIVED_CONTENT_CHANNEL = 'msgvault-archived-content';
export const MAX_ARCHIVED_SCROLL_DELTA = 10_000;
export const MAX_ARCHIVED_FRAME_HEIGHT = 65_536;

// The frame's executable and styling surface is exactly two same-origin
// static assets. Everything the frame needs beyond archived HTML travels as
// data attributes on <html>: the message-scoped bridge nonce, the validated
// shell origin messages are pinned to, and the rendering mode/scheme the
// stylesheet keys on.
//
// Static assets — not inline — because the daemon serves the shell with a
// Content-Security-Policy of script-src 'self'; style-src 'self', and
// sandboxed srcdoc frames inherit that policy in addition to their own meta
// CSP. Inline <script>/<style> (even hash-authorized in the meta CSP) is
// blocked by the inherited policy; same-origin URLs satisfy both.
export const ARCHIVED_FRAME_SCRIPT_PATH = '/archived-frame.js';
export const ARCHIVED_FRAME_STYLE_PATH = '/archived-frame.css';

export type FrameColorScheme = 'light' | 'dark';

export interface FrameAppearance {
  /** 'canvas': designed mail on its own white canvas. 'themed': simple mail
   * rendered transparently on the shell surface with shell inks. */
  mode: 'canvas' | 'themed';
  colorScheme: FrameColorScheme;
}

export interface FrameDocumentOptions {
  html: string;
  nonce: string;
  targetOrigin: string;
  remoteImages?: string[];
  appearance?: FrameAppearance;
}

function escapeAttribute(value: string): string {
  return value
    .replaceAll('&', '&amp;')
    .replaceAll('"', '&quot;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;');
}

function encodedBody(html: string): string {
  // Sanitization is the primary boundary. This is a small defense against a
  // caller accidentally passing a closing script/body sequence directly.
  return html
    .replace(/<\/script/gi, '&lt;/script')
    .replace(/<script/gi, '&lt;script')
    .replace(/<\/body/gi, '&lt;/body')
    .replace(/<\/html/gi, '&lt;/html');
}

function validImageSource(value: string): string | undefined {
  try {
    const url = new URL(value);
    if (url.protocol !== 'https:' && url.protocol !== 'http:') return undefined;
    if (url.username || url.password || /[;\u0000-\u001f\u007f]/.test(value)) return undefined;
    // Second layer behind the sanitizer's gate: a private destination that
    // slips into a caller's remoteImages list still never enters the frame's
    // img-src allowlist, so the browser refuses the fetch.
    if (prohibitedRemoteHost(url.hostname)) return undefined;
    url.hash = '';
    return url.toString();
  } catch {
    return undefined;
  }
}

function exactHTTPOrigin(value: string): string {
  let parsed: URL;
  try {
    parsed = new URL(value);
  } catch {
    throw new Error('Archived content bridge requires a valid HTTP origin');
  }
  if ((parsed.protocol !== 'http:' && parsed.protocol !== 'https:') || parsed.origin !== value) {
    throw new Error('Archived content bridge requires an exact origin');
  }
  return parsed.origin;
}

export async function buildFrameDocument(options: FrameDocumentOptions): Promise<string> {
  const appearance = options.appearance ?? { mode: 'canvas' as const, colorScheme: 'light' as const };
  const origin = exactHTTPOrigin(options.targetOrigin);
  const bridgeURL = `${origin}${ARCHIVED_FRAME_SCRIPT_PATH}`;
  const styleURL = `${origin}${ARCHIVED_FRAME_STYLE_PATH}`;
  const remoteSources = [...new Set((options.remoteImages ?? [])
    .map(validImageSource)
    .filter((source): source is string => source !== undefined))];
  const imageSources = ['data:', ...remoteSources].map(escapeAttribute).join(' ');
  const csp = [
    "default-src 'none'",
    "connect-src 'none'",
    `img-src ${imageSources}`,
    "media-src 'none'",
    "font-src data:",
    // Stylesheet elements stay pinned to the exact same-origin asset; style
    // attributes are allowed because they only ever carry declarations that
    // survived the sanitizer's inline-style allowlist (no url()/position/
    // expression surface). Browsers without -elem/-attr support fall back to
    // the strict style-src and simply render designed mail colorless.
    `style-src ${escapeAttribute(styleURL)}`,
    `style-src-elem ${escapeAttribute(styleURL)}`,
    "style-src-attr 'unsafe-inline'",
    `script-src ${escapeAttribute(bridgeURL)}`,
    "object-src 'none'",
    "frame-src 'none'",
    "base-uri 'none'",
    "form-action 'none'"
  ].join('; ');

  // Designed mail keeps its assumed white light canvas in both shell themes;
  // simple mail adopts the shell scheme and renders transparently on the
  // theme surface. archived-frame.css keys on the data attributes.
  return '<!doctype html>' +
    `<html data-bridge-nonce="${escapeAttribute(options.nonce)}"` +
    ` data-bridge-origin="${escapeAttribute(origin)}"` +
    ` data-mode="${appearance.mode}"` +
    ` data-scheme="${appearance.mode === 'canvas' ? 'light' : appearance.colorScheme}"><head>` +
    '<meta charset="utf-8">' +
    `<meta http-equiv="Content-Security-Policy" content="${escapeAttribute(csp)}">` +
    '<meta name="referrer" content="no-referrer">' +
    `<link rel="stylesheet" href="${escapeAttribute(styleURL)}">` +
    `</head><body>${encodedBody(options.html)}` +
    `<script src="${escapeAttribute(bridgeURL)}"></script></body></html>`;
}
