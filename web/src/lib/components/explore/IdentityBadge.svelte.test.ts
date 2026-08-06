import { render, screen } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';

import IdentityBadge from './IdentityBadge.svelte';

describe('IdentityBadge', () => {
  it('sorts and deduplicates sender and recipient matches with direction-specific labels', () => {
    render(IdentityBadge, {
      senderIdentities: ['z-send@example.test', 'a-send@example.test', 'z-send@example.test'],
      recipientIdentities: ['b-mask@example.test', 'a-mask@example.test', 'b-mask@example.test']
    });

    expect(screen.getAllByTestId('identity-badge').map((badge) => badge.textContent)).toEqual([
      'Sent via: a-send@example.test',
      'Sent via: z-send@example.test',
      'Via: a-mask@example.test',
      'Via: b-mask@example.test'
    ]);
  });

  it('renders no badge for empty matches', () => {
    const rendered = render(IdentityBadge, {
      senderIdentities: [],
      recipientIdentities: []
    });

    expect(rendered.container.textContent?.trim()).toBe('');
    expect(screen.queryByTestId('identity-badge')).toBeNull();
  });
});
