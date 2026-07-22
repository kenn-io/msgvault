import DOMPurify from 'dompurify';

import { sanitizeInlineStyle } from './style-allowlist';

export interface ArchivedHTMLSanitizeOptions {
  messageId: number;
  allowRemoteImages?: boolean;
}

export interface SanitizedArchivedHTML {
  html: string;
  remoteImages: string[];
  inlineImages: ArchivedInlineImage[];
  /** True when the sender authored a visual design (backgrounds or
   * multi-column layout tables). Designed mail renders on its own white
   * canvas; everything else inherits the shell theme. */
  designed: boolean;
}

export interface ArchivedInlineImage {
  cid: string;
  alt: string;
}

const EMAIL_TAGS = [
  'a', 'abbr', 'address', 'article', 'b', 'blockquote', 'br', 'caption', 'center', 'cite',
  'code', 'col', 'colgroup', 'dd', 'del', 'details', 'div', 'dl', 'dt', 'em', 'figcaption',
  'figure', 'font', 'footer', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'header', 'hr', 'i',
  'img', 'ins', 'kbd', 'li', 'main', 'mark', 'ol', 'p', 'pre', 'q', 's', 'samp',
  'section', 'small', 'span', 'strike', 'strong', 'sub', 'summary', 'sup', 'table',
  'tbody', 'td', 'tfoot', 'th', 'thead', 'tr', 'tt', 'u', 'ul', 'var'
];

const EMAIL_ATTRIBUTES = [
  'abbr', 'align', 'alt', 'bgcolor', 'border', 'cellpadding', 'cellspacing', 'class', 'color',
  'colspan', 'dir', 'height', 'href', 'id', 'lang', 'rowspan', 'src', 'style', 'title',
  'valign', 'width'
];

// Legacy presentational color attributes old marketing mail relies on: bgcolor
// on table structure, color on font tags. Values must be a hex color or a
// bare color name — never anything URL- or script-shaped.
const PRESENTATIONAL_COLOR = /^(?:#[0-9a-f]{3,8}|[a-z]+)$/i;
const BGCOLOR_ELEMENTS = new Set([
  'table', 'thead', 'tbody', 'tfoot', 'tr', 'td', 'th', 'col', 'colgroup'
]);

function keepPresentationalAttributes(element: HTMLElement): void {
  const style = element.getAttribute('style');
  if (style !== null) {
    const kept = sanitizeInlineStyle(style);
    if (kept === '') element.removeAttribute('style');
    else element.setAttribute('style', kept);
  }
  const bgcolor = element.getAttribute('bgcolor');
  if (bgcolor !== null && (!BGCOLOR_ELEMENTS.has(element.localName) ||
      !PRESENTATIONAL_COLOR.test(bgcolor.trim()))) {
    element.removeAttribute('bgcolor');
  }
  const color = element.getAttribute('color');
  if (color !== null && (element.localName !== 'font' ||
      !PRESENTATIONAL_COLOR.test(color.trim()))) {
    element.removeAttribute('color');
  }
}

const SAFE_DATA_IMAGE = /^data:image\/(?:gif|jpe?g|png|webp);base64,[a-z0-9+/=\s]+$/i;

/** Quote chains longer than this collapse behind a "Show quoted text" toggle. */
export const QUOTE_COLLAPSE_THRESHOLD = 300;

const QUOTE_ROOT_SELECTOR = 'blockquote, div[class*="gmail_quote"]';

const BACKGROUND_DECLARATION =
  /(?:^|;)\s*background(?:-color|-image)?\s*:\s*(?!(?:transparent|none|inherit|initial|unset)\s*(?:;|$))/i;

/**
 * Decides whether archived HTML mail carries an authored visual design.
 * Detection runs on the raw message HTML (parsed inertly, before
 * sanitization) so signals the sanitizer drops — style elements,
 * background-image declarations — still count.
 *
 * Designed mail: any background color/image declaration or a layout table
 * wider than one column. Text color alone is deliberately not a signal:
 * inline colors now survive sanitization, and a colored signature in an
 * otherwise plain reply must stay theme-native. Plain replies (paragraphs,
 * breaks, quote chains) stay theme-native.
 */
export function detectDesignedEmail(input: string): boolean {
  const parsed = new DOMParser().parseFromString(input, 'text/html');
  for (const style of parsed.querySelectorAll('style')) {
    if (/background(?:-color|-image)?\s*:/i.test(style.textContent ?? '')) return true;
  }
  for (const element of [parsed.body, ...parsed.body.querySelectorAll<HTMLElement>('*')]) {
    if (element.hasAttribute('bgcolor') || element.hasAttribute('background')) return true;
    if (BACKGROUND_DECLARATION.test(element.getAttribute('style') ?? '')) return true;
    if (element.localName === 'tr') {
      let cells = 0;
      for (const child of element.children) {
        if (child.localName === 'td' || child.localName === 'th') cells += 1;
      }
      if (cells >= 2) return true;
    }
  }
  return false;
}

function remoteImageURL(value: string): string | undefined {
  try {
    if (/[;\u0000-\u001f\u007f]/.test(value)) return undefined;
    const url = /^https?:\/\//i.test(value)
      ? new URL(value)
      : value.startsWith('//') ? new URL(`https:${value}`) : undefined;
    if (!url || (url.protocol !== 'https:' && url.protocol !== 'http:')) return undefined;
    if (url.username || url.password) return undefined;
    url.hash = '';
    return url.toString();
  } catch {
    return undefined;
  }
  return undefined;
}

export function imagePlaceholderBlock(document: Document, caption: string): HTMLElement {
  const placeholder = document.createElement('span');
  placeholder.setAttribute('data-archived-image-placeholder', '');
  const glyph = document.createElement('span');
  glyph.setAttribute('data-archived-image-glyph', '');
  glyph.setAttribute('aria-hidden', 'true');
  const label = document.createElement('span');
  label.setAttribute('data-archived-image-caption', '');
  label.textContent = caption;
  placeholder.append(glyph, label);
  return placeholder;
}

function blockedImage(document: Document, alt: string, index: number): HTMLElement {
  const placeholder = imagePlaceholderBlock(document, alt || 'Remote image');
  placeholder.setAttribute('data-archived-remote-image', String(index));
  placeholder.setAttribute('title', `Remote image not loaded${alt ? `: ${alt}` : ''}`);
  return placeholder;
}

function inlineImagePlaceholder(document: Document, alt: string, index: number): HTMLElement {
  const placeholder = document.createElement('span');
  placeholder.setAttribute('data-archived-inline-image', String(index));
  placeholder.textContent = `Inline image loading${alt ? `: ${alt}` : ''}`;
  return placeholder;
}

/**
 * Sanitizes sender-controlled archived HTML before it is put into srcdoc.
 * Remote URLs are returned to the shell separately and never retained in the
 * archived DOM until the user explicitly opts in.
 */
export function sanitizeArchivedHTML(
  input: string,
  options: ArchivedHTMLSanitizeOptions
): SanitizedArchivedHTML {
  const source = document.createElement('template');
  source.innerHTML = input;
  for (const container of source.content.querySelectorAll('svg, math')) {
    container.replaceWith(document.createTextNode(container.textContent ?? ''));
  }
  const sanitized = DOMPurify.sanitize(source.innerHTML, {
    ALLOWED_TAGS: EMAIL_TAGS,
    ALLOWED_ATTR: EMAIL_ATTRIBUTES,
    ALLOW_DATA_ATTR: false,
    ALLOW_ARIA_ATTR: false,
    FORBID_CONTENTS: ['script', 'style', 'template', 'noscript'],
    SANITIZE_NAMED_PROPS: true
  });
  const template = document.createElement('template');
  template.innerHTML = sanitized;
  const remoteImages: string[] = [];
  const inlineImages: ArchivedInlineImage[] = [];

  for (const element of template.content.querySelectorAll<HTMLElement>('*')) {
    for (const attribute of [...element.attributes]) {
      const name = attribute.name.toLowerCase();
      if (name.startsWith('on') || [
        'accesskey', 'autofocus', 'contenteditable', 'draggable', 'form', 'formaction',
        'name', 'nonce', 'ping', 'popover', 'srcdoc', 'srcset', 'tabindex', 'target'
      ].includes(name)) {
        element.removeAttribute(attribute.name);
      }
    }
    keepPresentationalAttributes(element);

    if (element instanceof HTMLAnchorElement) {
      const href = element.getAttribute('href')?.trim() ?? '';
      if (!href.startsWith('#')) element.removeAttribute('href');
    }
    if (!(element instanceof HTMLImageElement)) continue;

    const source = element.getAttribute('src')?.trim() ?? '';
    if (/^cid:/i.test(source)) {
      const cid = source.slice(source.indexOf(':') + 1);
      const alt = element.getAttribute('alt') ?? '';
      const index = inlineImages.push({ cid, alt }) - 1;
      element.replaceWith(inlineImagePlaceholder(document, alt, index));
      continue;
    }
    if (SAFE_DATA_IMAGE.test(source)) continue;
    const remote = remoteImageURL(source);
    if (remote) {
      const index = remoteImages.push(remote) - 1;
      if (options.allowRemoteImages) element.setAttribute('src', remote);
      else element.replaceWith(blockedImage(document, element.getAttribute('alt') ?? '', index));
      continue;
    }
    element.removeAttribute('src');
  }

  collapseQuoteChains(template.content);

  return {
    html: template.innerHTML,
    remoteImages,
    inlineImages,
    designed: detectDesignedEmail(input)
  };
}

/**
 * Marks top-level quote blocks (blockquote and Gmail quote containers) for
 * muted styling and folds long chains behind a native details/summary toggle
 * so the frame needs no additional script for Show/Hide quoted text.
 */
function collapseQuoteChains(content: DocumentFragment): void {
  for (const root of content.querySelectorAll<HTMLElement>(QUOTE_ROOT_SELECTOR)) {
    if (root.parentElement?.closest(QUOTE_ROOT_SELECTOR)) continue;
    root.setAttribute('data-archived-quote', '');
    const long = (root.textContent ?? '').trim().length > QUOTE_COLLAPSE_THRESHOLD ||
      root.querySelector(QUOTE_ROOT_SELECTOR) !== null;
    if (!long || root.closest('details') !== null) continue;
    const details = document.createElement('details');
    details.setAttribute('data-archived-quote-toggle', '');
    const summary = document.createElement('summary');
    const show = document.createElement('span');
    show.setAttribute('data-archived-quote-show', '');
    show.textContent = 'Show quoted text';
    const hide = document.createElement('span');
    hide.setAttribute('data-archived-quote-hide', '');
    hide.textContent = 'Hide quoted text';
    summary.append(show, hide);
    root.replaceWith(details);
    details.append(summary, root);
  }
}
