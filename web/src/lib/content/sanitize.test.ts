import { describe, expect, it } from 'vitest';

import { detectDesignedEmail, QUOTE_COLLAPSE_THRESHOLD, sanitizeArchivedHTML } from './sanitize';

// Real-world-shaped fixtures for the designed-mail heuristic and the reading
// affordances built on top of sanitization.
const MARKETING_EMAIL = `
  <html><head><style>.wrapper{background-color:#f4f4f4}</style></head>
  <body bgcolor="#f4f4f4">
  <table width="640" align="center"><tr>
    <td width="320" style="background-color:#ffffff;color:#111111">New drops</td>
    <td width="320"><img src="https://images.example/hero.png" alt="Hero" width="320"></td>
  </tr></table>
  </body></html>`;

const PLAIN_REPLY_EMAIL = `
  <div dir="ltr">Sounds good — let&#39;s do Tuesday.<br><br>Thanks!<div><br></div></div>
  <div class="gmail_quote">
    <div class="gmail_attr">On Mon, Jul 20, 2026 Alice &lt;alice@example.com&gt; wrote:</div>
    <blockquote class="gmail_quote">
      Are you free Tuesday or Wednesday next week? ${'We could also look at the following week. '.repeat(12)}
      <blockquote>Original scheduling note from way back.</blockquote>
    </blockquote>
  </div>`;

describe('sanitizeArchivedHTML', () => {
  it('removes active content, network-capable elements, and focus authority', () => {
    const result = sanitizeArchivedHTML(`
      <script>parent.postMessage({type:'close'}, '*')</script>
      <form action="https://collector.example/submit"><input autofocus name="secret"><button accesskey="x">Esc</button></form>
      <svg><a xlink:href="https://collector.example/svg"><text>SVG link</text></a></svg>
      <video poster="https://collector.example/poster"><source src="https://collector.example/movie"></video>
      <a href="https://collector.example/click" onclick="steal()" tabindex="0">External</a>
      <div role="button" contenteditable="true" onkeydown="steal()">Press Esc</div>
      <p style="background-image:url(https://collector.example/css)">Safe words</p>
      <style>@import 'https://collector.example/import';</style>
    `, { messageId: 42 });

    expect(result.html).toContain('Safe words');
    expect(result.html).toContain('Press Esc');
    expect(result.html).not.toMatch(/<(?:script|form|input|button|svg|video|source)\b|style=/i);
    expect(result.html).not.toMatch(/https?:|collector\.example|onclick|onkeydown|autofocus|accesskey|tabindex|contenteditable|role=/i);
    expect(result.remoteImages).toEqual([]);
  });

  it('removes every sender style without trying to parse hostile CSS', () => {
    const result = sanitizeArchivedHTML(`
      <p style="background:UrL(https://collector.example/mixed)">Mixed</p>
      <p style="background:u/**/rl(https://collector.example/comment)">Comment</p>
      <p style="background:\\75\\72\\6c(https://collector.example/escaped)">Escaped</p>
      <p style="background:&#x75;rl(https://collector.example/entity)">Encoded</p>
      <p style="@\\69mport 'https://collector.example/import'">Import</p>
      <p style="color: red">Benign looking</p>
    `, { messageId: 42 });

    expect(result.html).toContain('Benign looking');
    expect(result.html).not.toMatch(/style=|collector\.example|url\s*\(/i);
  });

  it('extracts CID images into URL-free shell descriptors', () => {
    const result = sanitizeArchivedHTML(
      '<img src="cid:logo/part@example.com" alt="Archived logo">',
      { messageId: 42 }
    );

    expect(result.html).toContain('Inline image loading: Archived logo');
    expect(result.html).not.toMatch(/cid:|\/api\/v1\/messages|logo\/part@example\.com/);
    expect(result.inlineImages).toEqual([{ cid: 'logo/part@example.com', alt: 'Archived logo' }]);
    expect(result.remoteImages).toEqual([]);
  });

  it('replaces remote images with URL-free consent placeholders', () => {
    const result = sanitizeArchivedHTML(`
      <img src="https://images.example/pixel.png?secret=1" alt="Chart">
      <img src="//images.example/second.png" alt="Second">
    `, { messageId: 42 });

    expect(result.remoteImages).toEqual([
      'https://images.example/pixel.png?secret=1',
      'https://images.example/second.png'
    ]);
    expect(result.html).toContain('data-archived-remote-image="0"');
    expect(result.html).toContain('data-archived-remote-image="1"');
    expect(result.html).not.toMatch(/images\.example|secret=1/);
  });

  it('renders blocked remote images as captioned placeholder blocks, not body copy', () => {
    const longAlt = 'Want to be the first to know about new drops? Subscribe now for updates.';
    const result = sanitizeArchivedHTML(`
      <img src="https://images.example/one.png" alt="${longAlt}">
      <img src="https://images.example/two.png" alt="Second">
      <img src="https://images.example/three.png">
      <img src="https://images.example/four.png" alt="Fourth">
    `, { messageId: 42 });

    const template = document.createElement('template');
    template.innerHTML = result.html;
    const placeholders = template.content.querySelectorAll('[data-archived-remote-image]');
    expect(placeholders).toHaveLength(4);
    for (const placeholder of placeholders) {
      expect(placeholder.hasAttribute('data-archived-image-placeholder')).toBe(true);
      expect(placeholder.querySelector('[data-archived-image-glyph]')).not.toBeNull();
      expect(placeholder.querySelector('[data-archived-image-caption]')).not.toBeNull();
    }
    // The alt text lives in the caption, never as free-flowing body prose.
    expect(template.content.querySelector('[data-archived-remote-image="0"]')?.textContent)
      .toBe(longAlt);
    expect(result.html).not.toContain('Remote image blocked:');
    // An alt-less image still gets a legible caption.
    expect(template.content.querySelector('[data-archived-remote-image="2"]')?.textContent)
      .toBe('Remote image');
  });

  it('allows only explicitly consented remote image URLs', () => {
    const result = sanitizeArchivedHTML(
      '<img src="https://images.example/chart.png" alt="Chart"><p style="background:url(https://tracker.example/bg)">Text</p>',
      { messageId: 42, allowRemoteImages: true }
    );

    expect(result.html).toContain('src="https://images.example/chart.png"');
    expect(result.html).not.toContain('tracker.example');
    expect(result.remoteImages).toEqual(['https://images.example/chart.png']);
  });

  it('handles malformed HTML without restoring stripped content', () => {
    const result = sanitizeArchivedHTML(
      '<div><p>Readable<script><img src=https://tracker.example/pixel></div><img src="javascript:alert(1)">',
      { messageId: 7 }
    );

    expect(result.html).toContain('Readable');
    expect(result.html).not.toMatch(/script|javascript:|tracker\.example/i);
  });

  it('collapses long quote chains behind a native Show quoted text toggle', () => {
    const result = sanitizeArchivedHTML(PLAIN_REPLY_EMAIL, { messageId: 42 });
    const template = document.createElement('template');
    template.innerHTML = result.html;

    const toggle = template.content.querySelector('details[data-archived-quote-toggle]');
    expect(toggle).not.toBeNull();
    expect(toggle?.querySelector('summary [data-archived-quote-show]')?.textContent)
      .toBe('Show quoted text');
    expect(toggle?.querySelector('summary [data-archived-quote-hide]')?.textContent)
      .toBe('Hide quoted text');
    // The whole Gmail quote container folds as one unit; nested quotes are
    // not double-wrapped.
    expect(toggle?.querySelector('[data-archived-quote]')?.classList.contains('gmail_quote'))
      .toBe(true);
    expect(template.content.querySelectorAll('details[data-archived-quote-toggle]')).toHaveLength(1);
    // The reply itself stays outside the fold.
    expect(template.content.firstElementChild?.textContent).toContain('Sounds good');
    expect(toggle?.textContent).not.toContain('Sounds good');
  });

  it('styles short quotes without hiding them behind a toggle', () => {
    const short = `<p>Agreed.</p><blockquote>${'a'.repeat(QUOTE_COLLAPSE_THRESHOLD - 1)}</blockquote>`;
    const result = sanitizeArchivedHTML(short, { messageId: 42 });
    const template = document.createElement('template');
    template.innerHTML = result.html;

    expect(template.content.querySelector('details[data-archived-quote-toggle]')).toBeNull();
    expect(template.content.querySelector('blockquote')?.hasAttribute('data-archived-quote'))
      .toBe(true);
  });
});

describe('detectDesignedEmail', () => {
  it('classifies marketing table mail as designed', () => {
    expect(detectDesignedEmail(MARKETING_EMAIL)).toBe(true);
    expect(sanitizeArchivedHTML(MARKETING_EMAIL, { messageId: 42 }).designed).toBe(true);
  });

  it('classifies plain replies with quote chains as theme-native', () => {
    expect(detectDesignedEmail(PLAIN_REPLY_EMAIL)).toBe(false);
    expect(sanitizeArchivedHTML(PLAIN_REPLY_EMAIL, { messageId: 42 }).designed).toBe(false);
  });

  it.each([
    ['bgcolor attribute', '<table><tr><td bgcolor="#ffffff">Only cell</td></tr></table>'],
    ['inline background style', '<div style="background:#101827"><p>Hero</p></div>'],
    ['background image style',
      '<table><tr><td style="background-image:url(banner.png)">Banner</td></tr></table>'],
    ['container text color', '<div style="color:#8899aa">Footer legalese</div>'],
    ['style-element background', '<style>.card{background-color:#fff}</style><p>Body</p>'],
    ['multi-column layout table', '<table><tr><td>Left nav</td><td>Right content</td></tr></table>']
  ])('treats %s as designed', (_name, html) => {
    expect(detectDesignedEmail(html)).toBe(true);
  });

  it.each([
    ['bare paragraphs and breaks', '<p>Hello,</p><br><div>See you then.<br>— Bob</div>'],
    ['inline span color only', '<p>Note: <span style="color:#1f497d">tracked change</span></p>'],
    ['single-column table', '<table><tr><td>One column of text</td></tr></table>'],
    ['transparent background', '<div style="background:transparent">Quoted</div>'],
    ['full plain document', '<html><body><p>Plain reply body.</p></body></html>']
  ])('keeps %s theme-native', (_name, html) => {
    expect(detectDesignedEmail(html)).toBe(false);
  });
});
