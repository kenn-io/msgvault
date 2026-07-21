import DOMPurify from 'dompurify';

export interface ArchivedHTMLSanitizeOptions {
  messageId: number;
  allowRemoteImages?: boolean;
}

export interface SanitizedArchivedHTML {
  html: string;
  remoteImages: string[];
  inlineImages: ArchivedInlineImage[];
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
  'abbr', 'align', 'alt', 'border', 'cellpadding', 'cellspacing', 'class', 'colspan',
  'dir', 'height', 'href', 'id', 'lang', 'rowspan', 'src', 'title', 'valign', 'width'
];

const SAFE_DATA_IMAGE = /^data:image\/(?:gif|jpe?g|png|webp);base64,[a-z0-9+/=\s]+$/i;

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

function blockedImage(document: Document, alt: string, index: number): HTMLElement {
  const placeholder = document.createElement('span');
  placeholder.setAttribute('data-archived-remote-image', String(index));
  placeholder.textContent = `Remote image blocked${alt ? `: ${alt}` : ''}`;
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
    element.removeAttribute('style');

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

  return { html: template.innerHTML, remoteImages, inlineImages };
}
