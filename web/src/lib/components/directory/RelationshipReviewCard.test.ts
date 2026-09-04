import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import type { RelationshipReviewRow } from '../../directory/relationship-review-controller.svelte';
import RelationshipReviewCard from './RelationshipReviewCard.svelte';

function row(overrides: Partial<RelationshipReviewRow> = {}): RelationshipReviewRow {
  return {
    id: 41,
    person_id: 7,
    matched_person_id: 9,
    raw_related_value: 'https://related.example.test/person/9',
    raw_related_type: 'friend',
    value_kind: 'uri',
    status: 'pending',
    source: 'vcard_import',
    created_at: '2026-08-01T10:00:00Z',
    updated_at: '2026-08-02T11:00:00Z',
    reviewed_at: '2026-08-03T12:00:00Z',
    ...overrides
  };
}

describe('RelationshipReviewCard', () => {
  it('renders every allow-listed field, keeps URI assertions plain, and opens durable profiles', async () => {
    const onOpenPerson = vi.fn();
    render(RelationshipReviewCard, { review: row(), onOpenPerson });

    const card = screen.getByRole('article', { name: 'Imported relationship review 41' });
    const terms = within(card).getAllByRole('term');
    expect(terms.map((term) => term.textContent)).toEqual([
      'Related value', 'Related type', 'Value kind', 'Status', 'Source', 'Created', 'Updated', 'Reviewed'
    ]);
    expect(terms.map((term) => term.nextElementSibling?.textContent)).toEqual([
      'https://related.example.test/person/9', 'friend', 'uri', 'pending', 'vcard_import',
      '2026-08-01T10:00:00Z', '2026-08-02T11:00:00Z', '2026-08-03T12:00:00Z'
    ]);
    expect(within(card).queryByRole('link')).toBeNull();
    expect(within(card).getByText('https://related.example.test/person/9').tagName).not.toBe('A');

    await fireEvent.click(within(card).getByRole('button', { name: 'Open owner profile' }));
    await fireEvent.click(within(card).getByRole('button', { name: 'Open matched profile' }));
    expect(onOpenPerson.mock.calls).toEqual([[7], [9]]);
    expect(within(card).queryByRole('button', { name: /accept|reject|unsure/i })).toBeNull();
  });

  it('omits reviewed time and invalid profile actions without inventing values', async () => {
    const onOpenPerson = vi.fn();
    render(RelationshipReviewCard, {
      review: row({ person_id: 0, matched_person_id: -9, reviewed_at: undefined }),
      onOpenPerson
    });

    expect(screen.queryByRole('term', { name: 'Reviewed' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Open owner profile' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Open matched profile' })).toBeNull();
    expect(onOpenPerson).not.toHaveBeenCalled();
  });

  it('never exposes excluded or unknown adversarial fields in DOM or accessible metadata', () => {
    const review = {
      ...row(),
      source_ref: 'forbidden-source-ref',
      source_resource_uid: 'forbidden-resource-uid',
      vcard_identity: {
        property: 'forbidden-vcard-property', group: 'forbidden-vcard-group',
        prop_id: 'forbidden-vcard-prop-id', pid: ['forbidden-vcard-pid'], altid: 'forbidden-vcard-altid'
      },
      created_by: 'forbidden-created-actor',
      reviewed_by: 'forbidden-reviewed-actor',
      accepted_relationship_id: 73,
      href: 'https://forbidden.example.test/review',
      authorization: 'forbidden-credential',
      raw_vcard: 'forbidden-raw-vcard',
      digest: 'forbidden-hash'
    };
    render(RelationshipReviewCard, { review, onOpenPerson: vi.fn() });

    expect(document.body.textContent).not.toMatch(/forbidden-/i);
    expect(document.body.innerHTML).not.toMatch(/forbidden-/i);
    expect(document.querySelector('[title]')).toBeNull();
    expect(screen.getAllByRole('button').map((button) => button.getAttribute('aria-label') ?? button.textContent))
      .not.toEqual(expect.arrayContaining([expect.stringMatching(/forbidden-/i)]));
  });
});
