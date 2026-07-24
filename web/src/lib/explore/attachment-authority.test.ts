import { describe, expect, it } from 'vitest';

import { attachmentSelection, parseAttachmentSelection } from './attachment-authority';

describe('attachment viewer authority', () => {
  it('uses a distinct namespaced selection and round-trips positive safe IDs', () => {
    expect(attachmentSelection(42)).toBe('attachment:42');
    expect(parseAttachmentSelection('attachment:42')).toBe(42);
  });

  it.each([
    null,
    'message:42',
    '42',
    'attachment:0',
    'attachment:-1',
    'attachment:1.5',
    'attachment:9007199254740992',
    'attachment:7:message:1'
  ])('rejects ambiguous or invalid authority %s', (value) => {
    expect(parseAttachmentSelection(value)).toBeUndefined();
  });
});
