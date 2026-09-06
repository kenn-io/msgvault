import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { FactLedgerController } from '../../directory/fact-ledger-controller.svelte';
import FactReviewPanel from './FactReviewPanel.svelte';

afterEach(() => cleanup());

describe('FactReviewPanel', () => {
  it('asks for a durable Directory person and remains network-silent without one', async () => {
    const fetchFn = vi.fn<typeof fetch>();
    const onOpenDirectory = vi.fn();
    const controller = new FactLedgerController(createAPIClient(fetchFn));

    render(FactReviewPanel, { controller, personID: null, onOpenDirectory });

    const panel = screen.getByRole('region', { name: 'Fact review' });
    expect(within(panel).getByText('Choose a person in Directory to inspect their fact ledger')).toBeDefined();
    await fireEvent.click(within(panel).getByRole('button', { name: 'Open Directory' }));
    expect(onOpenDirectory).toHaveBeenCalledOnce();
    expect(fetchFn).not.toHaveBeenCalled();
  });

  it('renders exact honest gates and selected durable-person navigation without decision controls', async () => {
    const controller = new FactLedgerController(createAPIClient(vi.fn<typeof fetch>()));
    controller.personID = 42;
    const onOpenPerson = vi.fn();
    render(FactReviewPanel, { controller, personID: 42, onOpenPerson });

    expect(screen.getByText('Person ID 42')).toBeDefined();
    expect(screen.getByText('Fact candidate decisions are unavailable until a generated candidate contract is installed.')).toBeDefined();
    expect(screen.getByText('A dated last-time-we-talked brief is unavailable until the server exposes a generated brief contract.')).toBeDefined();
    expect(screen.queryByRole('button', { name: /accept|reject|unsure|run/i })).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Open person profile' }));
    expect(onOpenPerson).toHaveBeenCalledWith(42);
  });
});
