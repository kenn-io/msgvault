import { createHash } from 'node:crypto';
import { describe, expect, it } from 'vitest';

import { archivedContentBridge, buildFrameDocument } from './frame-document';

describe('buildFrameDocument', () => {
  it('denies network and authorizes only the exact bridge script hash', async () => {
    const document = await buildFrameDocument({
      html: '<p>Archived content</p>',
      nonce: 'frame-nonce',
      targetOrigin: 'https://archive.example',
    });
    const digest = createHash('sha256').update(archivedContentBridge('https://archive.example')).digest('base64');

    expect(document).toContain("default-src 'none'");
    expect(document).toContain("connect-src 'none'");
    expect(document).toContain("media-src 'none'");
    expect(document).toContain(`script-src 'sha256-${digest}'`);
    expect(document).not.toContain("'unsafe-eval'");
    expect(document).not.toContain("script-src 'self'");
    expect(document).toContain('data-bridge-nonce="frame-nonce"');
    expect(document).toContain('const o="https://archive.example"');
    expect(document).not.toContain("postMessage({channel:c,nonce:n,type:'key',key:e.key},'*')");
  });

  it('allows only non-network data images unless remote consent is explicit', async () => {
    const blocked = await buildFrameDocument({
      html: '<p>Archived content</p>', nonce: 'n', targetOrigin: 'https://archive.example'
    });
    expect(blocked).toContain("img-src data:");
    expect(blocked).not.toContain('img-src data: https://archive.example');
    expect(blocked).not.toContain('images.example');

    const consented = await buildFrameDocument({
      html: '<img src="https://images.example/chart.png">',
      nonce: 'n',
      targetOrigin: 'https://archive.example',
      remoteImages: ['https://images.example/chart.png']
    });
    expect(consented).toContain('https://images.example/chart.png');
  });

  it('rejects CSP-delimiter, credentialed, and non-HTTP remote sources', async () => {
    const document = await buildFrameDocument({
      html: '<p>Archived content</p>',
      nonce: 'n',
      targetOrigin: 'https://archive.example',
      remoteImages: [
        'https://images.example/good.png?token=synthetic',
        'https://images.example/a.png; img-src https://collector.example',
        'https://user:pass@images.example/credential.png',
        'javascript:alert(1)'
      ]
    });

    expect(document).toContain('https://images.example/good.png?token=synthetic');
    expect(document).not.toContain('collector.example');
    expect(document).not.toContain('user:pass');
    expect(document).not.toContain('javascript:');
  });

  it('escapes shell-generated nonce and archived closing tags', async () => {
    const document = await buildFrameDocument({
      html: '<p title="</body><script>bad()</script>">Words</p>',
      nonce: '&quot; onload=bad()',
      targetOrigin: 'https://archive.example',
    });

    expect(document).toContain('data-bridge-nonce="&amp;quot; onload=bad()"');
    expect(document.match(/<script>/g)).toHaveLength(1);
  });

  it('rejects non-shell and injection-shaped target origins', async () => {
    await expect(buildFrameDocument({
      html: '<p>Words</p>', nonce: 'n', targetOrigin: 'null'
    })).rejects.toThrow('valid HTTP origin');
    await expect(buildFrameDocument({
      html: '<p>Words</p>', nonce: 'n', targetOrigin: 'https://archive.example/path'
    })).rejects.toThrow('exact origin');
  });
});
