export const ARCHIVED_CONTENT_CHANNEL = 'msgvault-archived-content';
export const MAX_ARCHIVED_SCROLL_DELTA = 10_000;

// This generated script is the only executable content admitted by the frame
// CSP. Its sole dynamic value is a validated, JSON-serialized shell origin.
export function archivedContentBridge(targetOrigin: string): string {
  return `(()=>{const n=document.documentElement.dataset.bridgeNonce;const c='${ARCHIVED_CONTENT_CHANNEL}';const o=${JSON.stringify(targetOrigin)};const keys=new Set(['Escape','PageUp','PageDown','Home','End','ArrowUp','ArrowDown']);addEventListener('keydown',e=>{if(keys.has(e.key))parent.postMessage({channel:c,nonce:n,type:'key',key:e.key},o)});addEventListener('wheel',e=>{if(Number.isFinite(e.deltaY)&&Math.abs(e.deltaY)<=${MAX_ARCHIVED_SCROLL_DELTA})parent.postMessage({channel:c,nonce:n,type:'scroll',deltaY:e.deltaY},o)},{passive:true})})()`;
}

export interface FrameDocumentOptions {
  html: string;
  nonce: string;
  targetOrigin: string;
  remoteImages?: string[];
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
    url.hash = '';
    return url.toString();
  } catch {
    return undefined;
  }
}

async function sha256Base64(value: string): Promise<string> {
  const bytes = new TextEncoder().encode(value);
  const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', bytes));
  let binary = '';
  for (const byte of digest) binary += String.fromCharCode(byte);
  return btoa(binary);
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
  const bridge = archivedContentBridge(exactHTTPOrigin(options.targetOrigin));
  const scriptHash = await sha256Base64(bridge);
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
    "style-src 'unsafe-inline'",
    `script-src 'sha256-${scriptHash}'`,
    "object-src 'none'",
    "frame-src 'none'",
    "base-uri 'none'",
    "form-action 'none'"
  ].join('; ');

  return '<!doctype html>' +
    `<html data-bridge-nonce="${escapeAttribute(options.nonce)}"><head>` +
    '<meta charset="utf-8">' +
    `<meta http-equiv="Content-Security-Policy" content="${escapeAttribute(csp)}">` +
    '<meta name="referrer" content="no-referrer">' +
    '<style>html{color-scheme:light dark}body{margin:0;padding:12px;overflow-wrap:anywhere;font:14px/1.5 system-ui,sans-serif}img{max-width:100%;height:auto}[data-archived-remote-image]{display:inline-block;padding:8px;border:1px dashed currentColor}</style>' +
    `</head><body>${encodedBody(options.html)}<script>${bridge}</script></body></html>`;
}
