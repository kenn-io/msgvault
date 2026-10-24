import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import OperationRelatedStatus from './OperationRelatedStatus.svelte';

afterEach(() => vi.unstubAllGlobals());

const visualStatus = {
  active_leases: 1, converged: 8, convergence_ratio: 0.8, convergence_total: 10,
  current: 7, duplicate_cost: { at_least_once: false, detail: 'bounded', provider_idempotent: true },
  eligible: 10, formats: [],
  generation: { consented: true, dimension: 3, fingerprint: 'private', id: 4, model: 'private', source_fence: 9, state: 'active' },
  journal_cursor: 8, journal_high_water: 10, journal_lag: 2, reconciliation_complete: false,
  retryable: 1, stale: 1, terminal: 1, tombstoned: 0, unavailable: 1, unknown_role: 0,
  usage: { billed_units: 0, input_bytes: 0, requests: 0, usage_available: false }
};

describe('OperationRelatedStatus', () => {
  it('fetches and renders the live visual attachment authority without private coordinates', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      requests.push(input instanceof Request ? input : new Request(input));
      return Response.json(visualStatus);
    });
    render(OperationRelatedStatus, {
      client: createAPIClient(fetchFn), authority: 'getVisualAttachmentStatus', configured: true, onClose: vi.fn(), onConfigure: vi.fn()
    });

    expect(await screen.findByText('7 of 10 attachments current')).toBeDefined();
    expect(requests.map((request) => new URL(request.url).pathname)).toEqual(['/api/v1/multimodal/status']);
    expect(document.body.textContent).not.toContain('private');
  });

  it('does not fetch unconfigured visual attachments and instead opens their Settings', async () => {
    const onConfigure = vi.fn();
    const fetchFn = vi.fn<typeof fetch>();
    render(OperationRelatedStatus, {
      client: createAPIClient(fetchFn), authority: 'getVisualAttachmentStatus', configured: false,
      onClose: vi.fn(), onConfigure
    });

    await fireEvent.click(await screen.findByRole('button', { name: 'Open visual attachment settings' }));
    expect(onConfigure).toHaveBeenCalledWith();
    expect(fetchFn).not.toHaveBeenCalled();
  });

  it('keeps endpoint failures on the status authority with fixed retry and no Settings redirect', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({
      error: 'private_error', message: 'private provider response'
    }, { status: 503 }));
    render(OperationRelatedStatus, {
      client: createAPIClient(fetchFn), authority: 'getVisualAttachmentStatus', configured: true,
      onClose: vi.fn(), onConfigure: vi.fn()
    });

    expect((await screen.findByRole('alert')).textContent).toContain('Unable to load visual attachment status.');
    expect(screen.getByRole('button', { name: 'Retry visual attachment status' })).toBeDefined();
    expect(screen.queryByRole('button', { name: /settings/i })).toBeNull();
    expect(document.body.textContent).not.toContain('private provider response');
    await fireEvent.click(screen.getByRole('button', { name: 'Retry visual attachment status' }));
    await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(2));
  });
});
