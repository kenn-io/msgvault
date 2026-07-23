import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

import {
  ARCHIVED_CONTENT_CHANNEL,
  ARCHIVED_FRAME_SCRIPT_PATH,
  ARCHIVED_FRAME_STYLE_PATH,
  buildFrameDocument,
  MAX_ARCHIVED_FRAME_HEIGHT,
  MAX_ARCHIVED_SCROLL_DELTA
} from './frame-document';

const publicDir = resolve(dirname(fileURLToPath(import.meta.url)), '../../../public');
const bridgeSource = readFileSync(resolve(publicDir, `.${ARCHIVED_FRAME_SCRIPT_PATH}`), 'utf8');
const frameStyles = readFileSync(resolve(publicDir, `.${ARCHIVED_FRAME_STYLE_PATH}`), 'utf8');

describe('buildFrameDocument', () => {
  it('denies network and authorizes only the same-origin bridge and stylesheet', async () => {
    const document = await buildFrameDocument({
      html: '<p>Archived content</p>',
      nonce: 'frame-nonce',
      targetOrigin: 'https://archive.example',
    });

    expect(document).toContain("default-src 'none'");
    expect(document).toContain("connect-src 'none'");
    expect(document).toContain("media-src 'none'");
    expect(document).toContain('script-src https://archive.example/archived-frame.js');
    expect(document).toContain('style-src https://archive.example/archived-frame.css');
    expect(document).toContain('style-src-elem https://archive.example/archived-frame.css');
    // Only style attributes get the inline allowance — they carry nothing but
    // declarations that survived the sanitizer's inline-style allowlist.
    expect(document).toContain("style-src-attr 'unsafe-inline'");
    expect(document).not.toMatch(/script-src[^;]*'unsafe-inline'/);
    expect(document).not.toMatch(/style-src(?:-elem)? [^;]*'unsafe-inline'/);
    expect(document).not.toContain("'unsafe-eval'");
    expect(document).not.toContain("script-src 'self'");
    expect(document).not.toMatch(/<script>|<style>/);
    expect(document).toContain('data-bridge-nonce="frame-nonce"');
    expect(document).toContain('data-bridge-origin="https://archive.example"');
  });

  it('never allowlists a remote origin for images — data: is the entire img-src', async () => {
    // Remote images arrive as data: URIs via the daemon's hardened proxy
    // (reader/remote-images.ts); the frame's browsing context must never be
    // able to contact a sender-controlled host, consented or not.
    const document = await buildFrameDocument({
      html: '<p>Archived content</p>', nonce: 'n', targetOrigin: 'https://archive.example'
    });
    const imgDirective = document
      .match(/content="([^"]*)"/)?.[1]
      ?.split(';')
      .find((directive) => directive.trim().startsWith('img-src'));
    expect(imgDirective?.trim()).toBe('img-src data:');
    expect(document).not.toContain('img-src data: https://archive.example');
    expect(document).not.toContain('images.example');
  });

  it('escapes shell-generated nonce and archived closing tags', async () => {
    const document = await buildFrameDocument({
      html: '<p title="</body><script>bad()</script>">Words</p>',
      nonce: '&quot; onload=bad()',
      targetOrigin: 'https://archive.example',
    });

    expect(document).toContain('data-bridge-nonce="&amp;quot; onload=bad()"');
    expect(document.match(/<script\b/g)).toHaveLength(1);
    expect(document).toContain('<script src="https://archive.example/archived-frame.js">');
  });

  it('rejects non-shell and injection-shaped target origins', async () => {
    await expect(buildFrameDocument({
      html: '<p>Words</p>', nonce: 'n', targetOrigin: 'null'
    })).rejects.toThrow('valid HTTP origin');
    await expect(buildFrameDocument({
      html: '<p>Words</p>', nonce: 'n', targetOrigin: 'https://archive.example/path'
    })).rejects.toThrow('exact origin');
  });

  it('renders designed mail on a light white canvas regardless of shell scheme', async () => {
    const document = await buildFrameDocument({
      html: '<table><tr><td>Designed</td></tr></table>',
      nonce: 'n',
      targetOrigin: 'https://archive.example',
      appearance: { mode: 'canvas', colorScheme: 'dark' }
    });

    expect(document).toContain('data-mode="canvas"');
    expect(document).toContain('data-scheme="light"');
  });

  it('defaults to the white canvas when no appearance is provided', async () => {
    const document = await buildFrameDocument({
      html: '<p>Words</p>', nonce: 'n', targetOrigin: 'https://archive.example'
    });

    expect(document).toContain('data-mode="canvas"');
    expect(document).toContain('data-scheme="light"');
  });

  it('renders simple mail in the shell scheme', async () => {
    const document = await buildFrameDocument({
      html: '<p>Simple</p>',
      nonce: 'n',
      targetOrigin: 'https://archive.example',
      appearance: { mode: 'themed', colorScheme: 'dark' }
    });

    expect(document).toContain('data-mode="themed"');
    expect(document).toContain('data-scheme="dark"');
  });
});

// The static assets are the deployable halves of the frame contract; these
// tests pin the parts the shell relies on so drift fails fast.
describe('archived frame static assets', () => {
  it('keeps the bridge channel and limits in sync with the shell constants', () => {
    expect(bridgeSource).toContain(`const channel = '${ARCHIVED_CONTENT_CHANNEL}';`);
    expect(bridgeSource).toContain(`const MAX_SCROLL_DELTA = ${MAX_ARCHIVED_SCROLL_DELTA};`);
    expect(bridgeSource).toContain(`const MAX_FRAME_HEIGHT = ${MAX_ARCHIVED_FRAME_HEIGHT};`);
  });

  it('pins bridge messages to a validated shell origin, never a wildcard', () => {
    expect(bridgeSource).not.toContain("'*'");
    expect(bridgeSource).toContain('dataset.bridgeOrigin');
    expect(bridgeSource).toContain('if (!nonce || !origin) return;');
  });

  it('ships author-overridable reading typography with both rendering modes', () => {
    expect(frameStyles).toContain(':where(body)');
    expect(frameStyles).toContain('font-size: 14px');
    expect(frameStyles).toContain('line-height: 1.55');
    expect(frameStyles).toContain("html[data-mode='canvas'] body");
    expect(frameStyles).toContain('max-width: 680px');
    expect(frameStyles).toContain("html[data-mode='themed'][data-scheme='dark']");
    expect(frameStyles).toContain('color-scheme: dark');
    expect(frameStyles).toContain('[data-archived-image-placeholder]');
    expect(frameStyles).toContain('max-height: 80px');
    expect(frameStyles).toContain('[data-archived-quote-hide]');
  });
});
