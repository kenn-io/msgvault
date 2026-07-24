import { describe, expect, it } from 'vitest';

import { prohibitedRemoteHost } from './url-safety';

describe('prohibitedRemoteHost', () => {
  const allowed = [
    'example.com',
    'images.example.com',
    'cdn.mail-host.co.uk',
    'example.com.',              // trailing FQDN dot on a public name
    'local.example.com',         // "local" as a non-final label
    'mylocal.com',               // suffix match is label-bounded
    'internal-tools.example.com',
    'notlocalhost.example.com',
    '8.8.8.8',
    '93.184.216.34',
    '9.255.255.255',             // just below 10/8
    '11.0.0.1',                  // just above 10/8
    '100.63.255.255',            // just below 100.64/10
    '100.128.0.1',               // just above 100.64/10
    '126.255.255.255',           // just below 127/8
    '128.0.0.1',                 // just above 127/8
    '169.253.0.1',               // adjacent to 169.254/16
    '172.15.255.255',            // just below 172.16/12
    '172.32.0.1',                // just above 172.16/12
    '192.167.1.1',               // adjacent to 192.168/16
    '192.169.1.1',
    '[2001:4860:4860::8888]',    // public IPv6
    '[2606:4700::6810:84e5]',
    '[64:ff9b::808:808]'         // NAT64 of a public address
  ];

  const blocked: Array<[string, string]> = [
    ['localhost', 'reserved loopback name'],
    ['LOCALHOST', 'case-insensitive'],
    ['localhost.', 'trailing dot'],
    ['app.localhost', '*.localhost'],
    ['printer.local', '*.local (mDNS)'],
    ['registry.internal', '*.internal'],
    ['home.arpa', 'RFC 8375 zone apex'],
    ['router.home.arpa', '*.home.arpa'],
    ['intranet', 'single-label hostname'],
    ['nas.', 'single label with trailing dot'],
    ['', 'empty host'],
    ['127.0.0.1', 'loopback'],
    ['127.255.255.255', 'end of 127/8'],
    ['0.0.0.0', 'unspecified'],
    ['0.1.2.3', '0/8 this-network'],
    ['10.0.0.1', 'RFC 1918 10/8'],
    ['10.255.255.255', 'end of 10/8'],
    ['100.64.0.1', 'CGNAT 100.64/10'],
    ['100.127.255.255', 'end of 100.64/10'],
    ['169.254.169.254', 'link-local metadata endpoint'],
    ['172.16.0.1', 'RFC 1918 172.16/12'],
    ['172.31.255.255', 'end of 172.16/12'],
    ['192.168.1.1', 'RFC 1918 192.168/16'],
    ['2130706433', 'decimal loopback (unnormalized)'],
    ['017700000001', 'octal loopback (unnormalized)'],
    ['0x7f000001', 'hex loopback (unnormalized)'],
    ['192.168.1.0x1', 'hex final label (unnormalized)'],
    ['[::1]', 'IPv6 loopback'],
    ['[::]', 'IPv6 unspecified'],
    ['[fc00::1]', 'ULA fc00::/7'],
    ['[fdab:1234::1]', 'ULA fd00::/8'],
    ['[fe80::1]', 'link-local fe80::/10'],
    ['[febf::1]', 'end of fe80::/10'],
    ['[::ffff:7f00:1]', 'IPv4-mapped loopback (hex groups)'],
    ['[::ffff:127.0.0.1]', 'IPv4-mapped loopback (dotted)'],
    ['[::ffff:192.168.1.1]', 'IPv4-mapped RFC 1918'],
    ['[::ffff:a9fe:a9fe]', 'IPv4-mapped link-local'],
    ['[::127.0.0.1]', 'IPv4-compatible loopback'],
    ['[not:an:address]', 'unparseable bracketed literal'],
    ['[::1%25eth0]', 'zone identifier']
  ];

  it.each(allowed)('allows public host %s', (host) => {
    expect(prohibitedRemoteHost(host)).toBe(false);
  });

  it.each(blocked)('blocks %s (%s)', (host) => {
    expect(prohibitedRemoteHost(host)).toBe(true);
  });

  it('blocks every WHATWG-normalized IPv4 obfuscation of loopback', () => {
    // The URL parser normalizes numeric hosts to dotted-quad before the gate
    // sees them; assert on the parser output end-to-end.
    for (const raw of [
      'http://2130706433/x.png',
      'http://017700000001/x.png',
      'http://0x7f000001/x.png',
      'http://0x7f.1/x.png',
      'http://127.1/x.png',
      'http://127.0.1/x.png'
    ]) {
      const { hostname } = new URL(raw);
      expect(hostname, raw).toBe('127.0.0.1');
      expect(prohibitedRemoteHost(hostname), raw).toBe(true);
    }
  });
});
