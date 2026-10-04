import type { Evidence, KataIssueCreateRequest, Selector } from '../api/generated/models';

export type EvidenceSelector = Selector;
export type PreparedEvidence = Evidence;

// Matches the server's evidence window; a passage never exceeds it.
export const EVIDENCE_WINDOW = 1000;
// Windows overlap by half, so any passage up to 500 characters appears whole in one.
export const EVIDENCE_STEP = EVIDENCE_WINDOW / 2;

// The retry key is derived from the request itself, so resubmitting the same
// input, even after a reload, replays the original issue instead of
// creating another; any edit, or a salt, yields a new key. Evidence counts by
// the passage the server prepared it as, which survives re-syncs that change
// only content hashes. The server compares a request hash under each key, so
// a hash collision surfaces as a conflict rather than wrong data; a
// non-cryptographic hash is enough and works where crypto.subtle is missing
// (plain-HTTP origins).
export function retryKey(request: KataIssueCreateRequest, evidence: Pick<PreparedEvidence, 'passage'>[], salt = ''): string {
  const passages = evidence.map(({ passage }) => passage).sort();
  const fingerprint = JSON.stringify([request.title, request.brief ?? '', request.list ?? '', passages, salt]);
  const bytes = new TextEncoder().encode(fingerprint);
  return 'web-' + fnv1a64(bytes, 0xcbf29ce484222325n) + fnv1a64(bytes, 0x84222325cbf29ce4n);
}

function fnv1a64(bytes: Uint8Array, seed: bigint): string {
  let hash = seed;
  for (const byte of bytes) {
    hash = BigInt.asUintN(64, (hash ^ BigInt(byte)) * 0x100000001b3n);
  }
  return hash.toString(16).padStart(16, '0');
}
