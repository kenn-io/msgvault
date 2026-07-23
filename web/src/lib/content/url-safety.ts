/**
 * Gate for sender-controlled remote fetch destinations in archived mail.
 *
 * A sender-authored `<img src>` may point a "Load images" fetch at services
 * the archive's browser can reach but the sender cannot: loopback daemons,
 * link-local metadata endpoints, RFC 1918 hosts on the user's LAN. The
 * sandbox blocks response reads, but a request alone can change state or
 * carry browser-managed credentials. This gate prohibits destinations that
 * are private by address or by reserved/conventional name, so consented
 * image loads may target only public-looking hosts.
 *
 * DNS rebinding — a public hostname whose A/AAAA record points at a private
 * address at fetch time — cannot be detected in the browser. That class is
 * closed by the daemon's hardened image proxy (GET
 * /api/v1/content/remote-image): consented remote images are fetched
 * server-side, where every resolved address and redirect hop is validated
 * and the connection is pinned to the validated IP. This browser gate
 * remains as a cheap defense-in-depth pre-filter for the direct-addressing
 * class, so prohibited URLs are never even offered for consent.
 */

const PROHIBITED_HOST_SUFFIXES = ['localhost', 'local', 'internal', 'home.arpa'];

const IPV4_HOST = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/;
const IPV6_GROUP = /^[0-9a-f]{1,4}$/;

function parseIPv4(host: string): number[] | undefined {
  const match = IPV4_HOST.exec(host);
  if (!match) return undefined;
  const octets = match.slice(1).map(Number);
  return octets.every((octet) => octet <= 255) ? octets : undefined;
}

function privateIPv4(octets: number[]): boolean {
  const [a = -1, b = -1] = octets;
  return a === 0 ||    // "this network" / unspecified — routes to loopback
    a === 10 ||        // RFC 1918
    a === 127 ||       // loopback
    (a === 100 && b >= 64 && b <= 127) ||  // 100.64/10 shared (CGNAT)
    (a === 169 && b === 254) ||            // link-local (cloud metadata)
    (a === 172 && b >= 16 && b <= 31) ||   // RFC 1918
    (a === 192 && b === 168);              // RFC 1918
}

/** Parses an IPv6 literal (no brackets) into eight 16-bit groups. */
function parseIPv6(literal: string): number[] | undefined {
  let hex = literal;
  const lastColon = hex.lastIndexOf(':');
  if (lastColon === -1) return undefined;
  const tail = hex.slice(lastColon + 1);
  if (tail.includes('.')) {
    // Trailing dotted quad (::ffff:127.0.0.1) becomes two hex groups so the
    // `::` expansion below sees a uniform grouped address.
    const octets = parseIPv4(tail);
    if (octets === undefined) return undefined;
    const [o0 = 0, o1 = 0, o2 = 0, o3 = 0] = octets;
    hex = `${hex.slice(0, lastColon + 1)}${((o0 << 8) | o1).toString(16)}:${((o2 << 8) | o3).toString(16)}`;
  }
  const halves = hex.split('::');
  if (halves.length > 2) return undefined;
  const groupsOf = (part: string): number[] | undefined => {
    if (part === '') return [];
    const groups: number[] = [];
    for (const token of part.split(':')) {
      if (!IPV6_GROUP.test(token)) return undefined;
      groups.push(Number.parseInt(token, 16));
    }
    return groups;
  };
  const front = groupsOf(halves[0] ?? '');
  const back = halves.length === 2 ? groupsOf(halves[1] ?? '') : [];
  if (front === undefined || back === undefined) return undefined;
  const missing = 8 - front.length - back.length;
  if (halves.length === 1 ? missing !== 0 : missing < 1) return undefined;
  return [...front, ...Array.from({ length: missing }, () => 0), ...back];
}

function privateIPv6(groups: number[]): boolean {
  const zeroThrough = (count: number): boolean =>
    groups.slice(0, count).every((group) => group === 0);
  const [first = 0] = groups;
  if (zeroThrough(7) && ((groups[7] ?? 0) <= 1)) return true;  // :: and ::1
  if ((first & 0xfe00) === 0xfc00) return true;                // fc00::/7 ULA
  if ((first & 0xffc0) === 0xfe80) return true;                // fe80::/10 link-local
  // IPv4-mapped (::ffff:0:0/96) and deprecated IPv4-compatible (::/96)
  // addresses defer to the embedded IPv4 rules.
  if (zeroThrough(5) && (groups[5] === 0xffff || groups[5] === 0)) {
    const high = groups[6] ?? 0;
    const low = groups[7] ?? 0;
    return privateIPv4([high >> 8, high & 0xff, low >> 8, low & 0xff]);
  }
  return false;
}

/**
 * Reports whether a URL hostname (as produced by the WHATWG URL parser,
 * which already normalizes decimal/octal/hex IPv4 obfuscations like
 * `2130706433`, `017700000001`, and `0x7f000001` to dotted-quad form) is a
 * prohibited remote-fetch destination: a loopback/private/link-local/
 * unspecified IP literal, a reserved or conventionally-private name
 * (`localhost`, `*.localhost`, `*.local`, `*.internal`, `*.home.arpa`), or
 * an unqualified single-label name.
 */
export function prohibitedRemoteHost(hostname: string): boolean {
  let host = hostname.trim().toLowerCase();
  if (host.endsWith('.')) host = host.slice(0, -1);
  if (host === '') return true;
  if (host.startsWith('[') || host.includes(':')) {
    const literal = host.startsWith('[') && host.endsWith(']') ? host.slice(1, -1) : host;
    const groups = parseIPv6(literal);
    return groups === undefined || privateIPv6(groups);
  }
  const octets = parseIPv4(host);
  if (octets !== undefined) return privateIPv4(octets);
  const labels = host.split('.');
  if (labels.length < 2) return true;
  // A numeric or hex final label marks an IPv4 form the URL parser would
  // normally normalize or reject; refuse any that arrives unnormalized.
  const last = labels[labels.length - 1] ?? '';
  if (/^\d+$/.test(last) || /^0x[0-9a-f]*$/.test(last)) return true;
  return PROHIBITED_HOST_SUFFIXES.some(
    (suffix) => host === suffix || host.endsWith(`.${suffix}`)
  );
}
