import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { defaultOperationsState, OperationsController } from '../../operations/controller.svelte';
import type { OperationsURLState } from '../../operations/models';
import { chooseSelectOption } from '../../../test/kit-ui';
import RunHistoryDialog from './RunHistoryDialog.svelte';

const run = { id: `op2.${'a'.repeat(32)}.synthetic`, kind: 'document_extraction', lane: 'documents', state: 'partial', started_at: '2026-10-28T11:00:00Z', finished_at: '2026-10-28T11:01:00Z', trigger: 'manual', counters: [], error: { code: 'provider_failed', message: 'Example provider failed' } };
const controllers: OperationsController[] = [];
afterEach(() => { for (const controller of controllers.splice(0)) controller.destroy(); });
function setup(kind: '' | 'source_sync' = '', filters: Partial<OperationsURLState> = {}) {
  const queries: URLSearchParams[] = [];
  let conflict = false;
  const client = createAPIClient(async input => {
    const url = new URL(input instanceof Request ? input.url : String(input));
    if (url.pathname.endsWith('/operations/status')) return Response.json({ lanes: [{ kind: 'document_extraction', lane: 'documents', configured: true, supported_actions: [], history_availability: 'available' }] });
    if (url.pathname === '/api/v1/operations/runs') {
      queries.push(url.searchParams);
      if (conflict && url.searchParams.has('cursor')) return Response.json({ error: 'operation_history_conflict', message: 'History changed' }, { status: 409 });
      return Response.json({ runs: url.searchParams.has('cursor') ? [{ ...run, id: `op2.${'b'.repeat(32)}.synthetic` }] : [run], membership_revision: 1, unavailable_kinds: [{ kind: 'person_embedding', code: 'unavailable' }], next_cursor: url.searchParams.has('cursor') ? undefined : 'next' });
    }
    if (url.pathname.startsWith('/api/v1/operations/runs/')) return Response.json({ ...run, related_status: 'getDocumentIndexStatus', supported_actions: [] });
    return Response.json({}, { status: 404 });
  });
  const controller = new OperationsController(client); controllers.push(controller);
  const onNavigate = vi.fn();
  render(RunHistoryDialog, { client, controller, state: { ...defaultOperationsState(kind), ...filters }, onNavigate, onclose: vi.fn(), onConfigure: vi.fn() });
  return { queries, onNavigate, drift: () => { conflict = true; } };
}

describe('Past runs', () => {
  it('sends inclusive selected dates as a UTC interval with an exclusive end', async () => {
    const { queries } = setup('', { operationStartedFrom: '2026-10-10T00:00:00Z', operationStartedBefore: '2026-10-13T00:00:00Z' });
    await screen.findByRole('button', { name: 'Open Document extraction run' });
    await fireEvent.click(screen.getByRole('button', { name: /2026-10-10/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Oct 11, 2026' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Oct 13, 2026' }));
    await waitFor(() => expect(queries.at(-1)?.get('started_from')).toBe('2026-10-11T00:00:00Z'));
    expect(queries.at(-1)?.get('started_before')).toBe('2026-10-14T00:00:00Z');
  });
  it('seeds the kind, changes filters, pages, and preserves unavailable kinds', async () => {
    const { queries } = setup('source_sync');
    await screen.findByRole('button', { name: 'Open Document extraction run' });
    expect(queries[0]?.get('kind')).toBe('source_sync');
    expect(screen.getByText('Person embedding history is unavailable.')).toBeDefined();
    await chooseSelectOption(screen.getByRole('combobox', { name: /^Kind:/ }), 'Document extraction');
    await waitFor(() => expect(queries.at(-1)?.get('kind')).toBe('document_extraction'));
    await chooseSelectOption(screen.getByRole('combobox', { name: /^State:/ }), 'Partial');
    await waitFor(() => expect(queries.at(-1)?.get('state')).toBe('partial'));
    await fireEvent.click(await screen.findByRole('button', { name: 'Show more' }));
    await waitFor(() => expect(screen.getAllByRole('button', { name: 'Open Document extraction run' })).toHaveLength(2));
    expect(queries.at(-1)?.get('cursor')).toBe('next');
  });
  it('offers Restart when page membership changes', async () => {
    const { drift, queries } = setup();
    await screen.findByRole('button', { name: 'Show more' }); drift();
    await fireEvent.click(await screen.findByRole('button', { name: 'Show more' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Restart' }));
    await waitFor(() => expect(queries.at(-1)?.has('cursor')).toBe(false));
  });
  it('opens detail, routes related status, and returns focus to the row', async () => {
    const { onNavigate } = setup();
    const row = await screen.findByRole('button', { name: 'Open Document extraction run' });
    row.focus(); await fireEvent.click(row);
    await screen.findByRole('region', { name: 'Operation run detail' });
    await fireEvent.click(screen.getByRole('button', { name: 'Open Document index status' }));
    expect(onNavigate).toHaveBeenCalledWith('getDocumentIndexStatus');
    await fireEvent.click(screen.getByRole('button', { name: 'All runs' }));
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Open Document extraction run' })));
  });
});

it('filters all document runs through Kind without adding a fourth control', async () => {
  const { queries } = setup();
  await screen.findByRole('button', { name: 'Open Document extraction run' });
  await chooseSelectOption(screen.getByRole('combobox', { name: /^Kind:/ }), 'All documents');
  await waitFor(() => expect(queries.at(-1)?.get('lane')).toBe('documents'));
  expect(queries.at(-1)?.has('kind')).toBe(false);
  await chooseSelectOption(screen.getByRole('combobox', { name: /^Kind:/ }), 'CardDAV sync');
  await waitFor(() => expect(queries.at(-1)?.get('kind')).toBe('carddav_sync'));
  expect(queries.at(-1)?.has('lane')).toBe(false);
});

it('refreshes on reopening and retries an unreadable history through Refresh', async () => {
  let failed = false;
  let runs = [run];
  const client = createAPIClient(async input => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
    if (path.endsWith('/status')) return Response.json({ lanes: [] });
    return failed ? Response.json({}, { status: 500 }) : Response.json({ runs, membership_revision: 1, unavailable_kinds: [] });
  });
  const controller = new OperationsController(client); controllers.push(controller);
  const props = { client, controller, state: defaultOperationsState(), onNavigate: vi.fn(), onclose: vi.fn(), onConfigure: vi.fn() };
  const first = render(RunHistoryDialog, props);
  await screen.findByRole('button', { name: 'Open Document extraction run' });
  first.unmount();
  runs = [{ ...run, kind: 'source_sync' }];
  failed = true;
  render(RunHistoryDialog, props);
  await screen.findByText('Unable to load operation history.');
  failed = false;
  await fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
  await screen.findByRole('button', { name: 'Open Source sync run' });
  expect(screen.queryByText('Unable to load operation history.')).toBeNull();
});

it('retries the selected run detail through Refresh', async () => {
  let failed = true;
  const client = createAPIClient(async input => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
    if (path.endsWith('/status')) return Response.json({ lanes: [] });
    if (path.endsWith('/runs')) return Response.json({ runs: [run], membership_revision: 1, unavailable_kinds: [] });
    return failed ? Response.json({}, { status: 500 }) : Response.json({ ...run, supported_actions: [] });
  });
  const controller = new OperationsController(client); controllers.push(controller);
  render(RunHistoryDialog, { client, controller, state: { ...defaultOperationsState(), operationRunID: run.id }, onNavigate: vi.fn(), onclose: vi.fn(), onConfigure: vi.fn() });
  await screen.findByText('Unable to load operation detail.');
  failed = false;
  await fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
  await screen.findByRole('region', { name: 'Operation run detail' });
  expect(screen.queryByText('Unable to load operation detail.')).toBeNull();
});
