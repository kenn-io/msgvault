import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { defaultOperationsState, OperationsController } from '../../operations/controller.svelte';
import type { OperationsURLState } from '../../operations/models';
import { chooseSelectOption } from '../../../test/kit-ui';
import RunHistoryDialog from './RunHistoryDialog.svelte';
import publicOperations from '../../../../tests/e2e/fixtures/operations-public.json';
import type { OperationRunsResponse } from '../../api/generated/models';

const fixture: OperationRunsResponse = { ...publicOperations as OperationRunsResponse, runs: (publicOperations as OperationRunsResponse).runs.map(candidate => ({ ...candidate, started_at: '2026-10-12T12:00:00Z', ...(candidate.finished_at ? { finished_at: '2026-10-12T12:01:00Z' } : {}) })) };
const run = fixture.runs.find(run => run.kind === 'document_extraction')!;
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
      const rows = [run, ...fixture.runs.filter(candidate => candidate.id !== run.id)]
        .filter(candidate => (!url.searchParams.get('kind') || candidate.kind === url.searchParams.get('kind')) &&
          (!url.searchParams.get('lane') || candidate.lane === url.searchParams.get('lane')) &&
          (!url.searchParams.get('state') || candidate.state === url.searchParams.get('state')) &&
          (!url.searchParams.get('started_from') || candidate.started_at >= url.searchParams.get('started_from')!) &&
          (!url.searchParams.get('started_before') || candidate.started_at < url.searchParams.get('started_before')!));
      return Response.json({ runs: url.searchParams.has('cursor') ? rows.slice(1) : rows.slice(0, 1), membership_revision: 1, unavailable_kinds: fixture.unavailable_kinds, next_cursor: !url.searchParams.has('cursor') && rows.length > 1 ? 'next' : undefined });
    }
    if (url.pathname.startsWith('/api/v1/operations/runs/')) return Response.json({ ...run, related_status: 'getDocumentIndexStatus', supported_actions: [] });
    return Response.json({}, { status: 404 });
  });
  const controller = new OperationsController(client); controllers.push(controller);
  const onNavigate = vi.fn();
  render(RunHistoryDialog, { client, controller, restorationEpoch: 0, state: { ...defaultOperationsState(kind), ...filters }, onNavigate, onclose: vi.fn(), onConfigure: vi.fn() });
  return { queries, onNavigate, drift: () => { conflict = true; } };
}

describe('Past runs', () => {
  it('maps selected dates, kind, lane, state, and cursor to history requests', async () => {
    const { queries } = setup('source_sync', { operationStartedFrom: '2026-10-10T00:00:00Z', operationStartedBefore: '2026-10-13T00:00:00Z' });
    await screen.findByRole('button', { name: 'Open Source sync run' });
    expect(queries[0]?.get('kind')).toBe('source_sync');
    await fireEvent.click(screen.getByRole('button', { name: /2026-10-10/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Oct 11, 2026' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Oct 13, 2026' }));
    await waitFor(() => expect(queries.at(-1)?.get('started_from')).toBe('2026-10-11T00:00:00Z'));
    expect(queries.at(-1)?.get('started_before')).toBe('2026-10-14T00:00:00Z');
    await chooseSelectOption(screen.getByRole('combobox', { name: /^Kind:/ }), 'All documents');
    await waitFor(() => expect(queries.at(-1)?.get('lane')).toBe('documents'));
    expect(queries.at(-1)?.has('kind')).toBe(false);
    await chooseSelectOption(screen.getByRole('combobox', { name: /^Kind:/ }), 'CardDAV sync');
    await waitFor(() => expect(queries.at(-1)?.get('kind')).toBe('carddav_sync'));
    expect(queries.at(-1)?.has('lane')).toBe(false);
    await chooseSelectOption(screen.getByRole('combobox', { name: /^State:/ }), 'Partial');
    await waitFor(() => expect(queries.at(-1)?.get('state')).toBe('partial'));
    await chooseSelectOption(screen.getByRole('combobox', { name: /^Kind:/ }), 'All kinds');
    await chooseSelectOption(screen.getByRole('combobox', { name: /^State:/ }), 'All states');
    await fireEvent.click(await screen.findByRole('button', { name: 'Show more' }));
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

it('refreshes on reopening and retries an unreadable history through Refresh', async () => {
  let failed = false;
  let runs = [run];
  const client = createAPIClient(async input => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
    if (path.endsWith('/status')) return Response.json({ lanes: [] });
    return failed ? Response.json({}, { status: 500 }) : Response.json({ runs, membership_revision: 1, unavailable_kinds: [] });
  });
  const controller = new OperationsController(client); controllers.push(controller);
  const props = { client, controller, restorationEpoch: 0, state: defaultOperationsState(), onNavigate: vi.fn(), onclose: vi.fn(), onConfigure: vi.fn() };
  const first = render(RunHistoryDialog, props);
  await screen.findByRole('button', { name: 'Open Document extraction run' });
  first.unmount();
  runs = [fixture.runs.find(run => run.kind === 'source_sync')!];
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
  render(RunHistoryDialog, { client, controller, restorationEpoch: 0, state: { ...defaultOperationsState(), operationRunID: run.id }, onNavigate: vi.fn(), onclose: vi.fn(), onConfigure: vi.fn() });
  await screen.findByText('Unable to load operation detail.');
  failed = false;
  await fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
  await screen.findByRole('region', { name: 'Operation run detail' });
  expect(screen.queryByText('Unable to load operation detail.')).toBeNull();
});

it('restores older archived runs after narrowing dates through All or Clear', async () => {
  const oldRun = { ...fixture.runs.find(run => run.kind === 'source_sync')!, started_at: '2020-01-01T12:00:00Z', finished_at: '2020-01-01T12:01:00Z' };
  const client = createAPIClient(async input => {
    const url = new URL((input as Request).url);
    if (url.pathname.endsWith('/status')) return Response.json({ lanes: [] });
    const from = url.searchParams.get('started_from');
    const before = url.searchParams.get('started_before');
    const matches = (!from || oldRun.started_at >= from) && (!before || oldRun.started_at < before);
    return Response.json({ runs: matches ? [oldRun] : [], membership_revision: 1, unavailable_kinds: [] });
  });
  const controller = new OperationsController(client); controllers.push(controller);
  render(RunHistoryDialog, { client, controller, restorationEpoch: 0, state: defaultOperationsState(), onNavigate: vi.fn(), onclose: vi.fn(), onConfigure: vi.fn() });
  await screen.findByRole('button', { name: 'Open Source sync run' });
  await fireEvent.click(screen.getByRole('button', { name: 'Custom range' }));
  await fireEvent.click(screen.getByRole('radio', { name: 'Relative' }));
  await fireEvent.click(screen.getByRole('button', { name: '7d' }));
  await screen.findByText('No runs match.');
  await fireEvent.click(screen.getByRole('button', { name: 'All' }));
  await screen.findByRole('button', { name: 'Open Source sync run' });
  await fireEvent.click(screen.getByRole('button', { name: '7d' }));
  await screen.findByText('No runs match.');
  await fireEvent.keyDown(document, { key: 'Escape' });
  await fireEvent.click(screen.getByRole('button', { name: 'Clear operation dates' }));
  await screen.findByRole('button', { name: 'Open Source sync run' });
});
