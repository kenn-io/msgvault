import { describe, expect, it } from 'vitest';

import { sanitizeArchivedHTML } from './sanitize';

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
    expect(result.html).toContain('Remote image blocked: Chart');
    expect(result.html).toContain('Remote image blocked: Second');
    expect(result.html).not.toMatch(/images\.example|secret=1/);
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
});
