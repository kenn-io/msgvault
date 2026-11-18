import { render, screen } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import AttachmentRail from './AttachmentRail.svelte';

describe('AttachmentRail', () => {
  it('omits the current year from file dates', () => {
    const year = new Date().getFullYear();
    render(AttachmentRail, { files: [{ key: 'one', id: 1, entry_key: 'one', message_id: 1, conversation_id: 1, source_id: 1, filename: 'notes.txt', mime_type: 'text/plain', size: 42, occurred_at: `${year}-05-03T12:00:00Z`, title: 'Notes', source_identifier: 'example' }] });
    expect(screen.getByText('May 3')).toBeDefined();
  });
});
