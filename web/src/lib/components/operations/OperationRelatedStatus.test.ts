import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import OperationRelatedStatus from './OperationRelatedStatus.svelte';

afterEach(() => vi.unstubAllGlobals());

const documentStatus = {
  status: {
    average_provider_latency_millis: 4, eligible_bytes: 100, eligible_occurrences: 6,
    eligible_owners: 5, exact_consent: true, extraction_attempts: 5, failed_attempts: 1,
    ineligible_role_occurrences: 0, missing_owners: 1, missing_provider_byte_reports: 0,
    processed_provider_units: 5, profile_enabled: true, profile_exists: true,
    provider_latency_millis: 20, provider_requests: 5, provider_retries: 1, ready_owners: 4,
    reported_provider_bytes: 100, retry_owners: 1, staging_owners: 0,
    stored_plaintext_chunks: 12, stored_plaintext_ranges: 0, successful_attempts: 4, terminal_owners: 0,
    unknown_role_occurrences: 0, verified_upload_bytes: 100
  },
  active_rebuild: { snapshot_owners: 5, remaining_owners: 1 }
};

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
  it('shows saved page ranges before any chunks are published', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({
      ...documentStatus,
      status: { ...documentStatus.status, stored_plaintext_chunks: 0, stored_plaintext_ranges: 2 }
    }));
    render(OperationRelatedStatus, {
      client: createAPIClient(fetchFn), authority: 'getDocumentIndexStatus', configured: true,
      onClose: vi.fn(), onConfigure: vi.fn()
    });

    expect(await screen.findByText('0 stored plaintext chunks · 2 saved page ranges · 5 provider requests')).toBeDefined();
  });

  it.each([
    ['getDocumentIndexStatus', '/api/v1/documents/status/current', documentStatus, '4 of 5 owners ready'],
    ['getDocumentVectorStatus', '/api/v1/documents/vectors/status', {
      enabled: true, configured: true, status: {
        configured_document_egress_fingerprint: 'private', configured_query_egress_fingerprint: 'private',
        configured_spec: { dimension: 3, embedding_profile: 'private', fingerprint: 'private', model: 'private', target_extraction_profile_id: 'private' },
        coverage: { ready: 7, required: 9 }, usage: { fingerprint: 'private', provider_calls: 2, provider_chunks: 7, provider_documents: 4, provider_input_chars: 90 }
      }
    }, '7 of 9 chunks ready'],
    ['getVisualAttachmentStatus', '/api/v1/multimodal/status', visualStatus, '7 of 10 attachments current']
  ] as const)('fetches and renders the live %s authority without private coordinates', async (
    authority, expectedPath, response, expectedCopy
  ) => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      requests.push(input instanceof Request ? input : new Request(input));
      return Response.json(response);
    });
    render(OperationRelatedStatus, {
      client: createAPIClient(fetchFn), authority, configured: true, onClose: vi.fn(), onConfigure: vi.fn()
    });

    expect(await screen.findByText(expectedCopy)).toBeDefined();
    expect(requests.map((request) => new URL(request.url).pathname)).toEqual([expectedPath]);
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

  it.each([
    ['getDocumentIndexStatus', 'Configured in config.toml on the daemon host.', 'Document indexing setup'],
    ['getDocumentVectorStatus', 'Configured in config.toml on the daemon host. Also needs semantic search.',
      'Document search setup']
  ] as const)('explains host configuration for unconfigured %s without a request', async (
    authority, text, linkName
  ) => {
    const fetchFn = vi.fn<typeof fetch>();
    render(OperationRelatedStatus, {
      client: createAPIClient(fetchFn), authority, configured: false, onClose: vi.fn(), onConfigure: vi.fn()
    });

    expect(await screen.findByText('Off')).toBeDefined();
    expect(screen.getByText(text, { exact: false })).toBeDefined();
    const link = screen.getByRole('link', { name: linkName });
    expect(link.getAttribute('target')).toBe('_blank');
    expect(link.getAttribute('rel')).toBe('noreferrer');
    expect(screen.queryByRole('button', { name: /settings/i })).toBeNull();
    expect(fetchFn).not.toHaveBeenCalled();
  });

  it.each([
    ['getDocumentIndexStatus', {
      ...documentStatus, status: { ...documentStatus.status, profile_enabled: false }
    }, 'Document indexing setup',
    'https://msgvault.io/docs/usage/document-indexing/#configure-the-policy'],
    ['getDocumentVectorStatus', { enabled: false, configured: true }, 'Document search setup',
      'https://msgvault.io/docs/usage/document-indexing/#semantic-and-hybrid-document-search']
  ] as const)('links %s to its host setup guide when the status needs configuration', async (
    authority, response, linkName, href
  ) => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json(response));
    render(OperationRelatedStatus, {
      client: createAPIClient(fetchFn), authority, configured: true, onClose: vi.fn(), onConfigure: vi.fn()
    });

    const link = await screen.findByRole('link', { name: linkName });
    expect(link.getAttribute('href')).toBe(href);
    expect(link.getAttribute('target')).toBe('_blank');
    expect(link.getAttribute('rel')).toBe('noreferrer');
    expect(screen.queryByRole('button', { name: /settings/i })).toBeNull();
  });

  it('omits the host setup guide when document status is fully configured', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json(documentStatus));
    render(OperationRelatedStatus, {
      client: createAPIClient(fetchFn), authority: 'getDocumentIndexStatus', configured: true,
      onClose: vi.fn(), onConfigure: vi.fn()
    });

    expect(await screen.findByText('4 of 5 owners ready')).toBeDefined();
    expect(screen.queryByRole('link', { name: 'Document indexing setup' })).toBeNull();
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
