import { afterEach, describe, expect, it, vi } from 'vitest';
import { retryKey } from './evidence';

const reference = {
  version: 1, kind: 'document_chunk' as const, archive_uid: 'archive-example', message_id: 42, source_type: 'email',
  source_identifier: 'archive@example.com', source_message_id: 'message-example', attachment_id: 3, occurrence_key: 'occurrence-example'
};
const request = { title: 'Follow up', list: 'agenda', evidence: [reference] };
const passage = { passage: 'p'.repeat(64) };

describe('retryKey', () => {
  afterEach(() => vi.unstubAllGlobals());

  it.each([
    { name: 'the same request', request, evidence: [passage], salt: '', same: true },
    { name: 'the same passage under a new attachment row', request: { ...request, evidence: [{ ...reference, attachment_id: 9 }] }, evidence: [passage], salt: '', same: true },
    { name: 'an edited title', request: { ...request, title: 'Edited' }, evidence: [passage], salt: '', same: false },
    { name: 'a salt', request, evidence: [passage], salt: 'salt', same: false },
    { name: 'another passage', request, evidence: [{ ...passage, passage: 'q'.repeat(64) }], salt: '', same: false },
  ])('keys $name as same=$same, without crypto.subtle', ({ request: changed, evidence, salt, same }) => {
    vi.stubGlobal('crypto', {});
    const key = retryKey(request, [passage]);
    expect(key).toMatch(/^web-[0-9a-f]{32}$/);
    expect(retryKey(changed, evidence, salt) === key).toBe(same);
  });
});
