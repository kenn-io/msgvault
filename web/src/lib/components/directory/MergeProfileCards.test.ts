import { render, screen, within } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import MergeProfileCards from './MergeProfileCards.svelte';

describe('MergeProfileCards', () => {
  it('shows each address once when either heading names that address', () => {
    render(MergeProfileCards, {
      label: 'Merged profiles',
      from: { title: 'mira.work@example.org', addresses: [{ id: '1', value: 'mira.work@example.org' }] },
      to: { title: 'Slack · neri · workspace: garden-club', addresses: [{ id: '2', value: 'Slack', detail: 'neri · workspace: garden-club' }, { id: '3', value: 'Slack', detail: 'bloom · workspace: seed-exchange' }, { id: '4', value: 'Slack', detail: 'neri · workspace: garden-club' }] },
    });
    const cards = within(screen.getByRole('group', { name: 'Merged profiles' })).getAllByRole('region');
    expect(cards[0].textContent?.trim()).toBe(within(cards[0]).getByRole('heading').textContent);
    expect(within(cards[1]).getByText('bloom · workspace: seed-exchange')).toBeDefined();
    expect(within(cards[1]).getAllByText('Slack', { exact: true })).toHaveLength(2);
  });

  it('keeps an empty line when the profile has no addresses', () => {
    render(MergeProfileCards, { label: 'Merged profiles', from: { title: 'Mira Vale', addresses: [] }, to: { title: 'Neri Bloom', addresses: [] } });
    expect(screen.getAllByText('No addresses')).toHaveLength(2);
  });
});
