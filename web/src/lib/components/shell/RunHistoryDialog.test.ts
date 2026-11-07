import { lane, lanes, visualStatus } from '../../../test/operations';
import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { defaultOperationsState, OperationsController } from '../../operations/controller.svelte';
import type { OperationsURLState } from '../../operations/models';
import { chooseSelectOption } from '../../../test/kit-ui';
import RunHistoryDialog from './RunHistoryDialog.svelte';
import publicOperations from '../../../../tests/e2e/fixtures/operations-public.json';
import type { OperationRunsResponse } from '../../api/generated/models';

const fixture: OperationRunsResponse = { ...publicOperations as OperationRunsResponse, runs: (publicOperations as OperationRunsResponse).runs.map(candidate => ({ ...candidate, started_at: '2026-10-12T12:00:00Z', ...(candidate.finished_at ? { finished_at: '2026-10-12T12:01:00Z' } : {}) })) };
const run = fixture.runs.find(run => run.kind === 'visual_embedding')!;
const controllers: OperationsController[] = [];
afterEach(() => { for (const controller of controllers.splice(0)) controller.destroy(); vi.useRealTimers(); });
function setup(kind: '' | 'source_sync' = '', filters: Partial<OperationsURLState> = {}) {
  let conflict = false;
  const client = createAPIClient(async input => {
    const url = new URL(input instanceof Request ? input.url : String(input));
    if (url.pathname.endsWith('/operations/status')) return Response.json({ lanes: lanes(lane('visual_embedding', { related_status: 'getVisualAttachmentStatus' })) });
    if (url.pathname === '/api/v1/operations/runs') {
      if (conflict && url.searchParams.has('cursor')) return Response.json({ error: 'operation_history_conflict', message: 'History changed' }, { status: 409 });
      const rows = [run, ...fixture.runs.filter(candidate => candidate.id !== run.id)]
        .filter(candidate => (!url.searchParams.get('kind') || candidate.kind === url.searchParams.get('kind')) &&
          (!url.searchParams.get('lane') || candidate.lane === url.searchParams.get('lane')) &&
          (!url.searchParams.get('state') || candidate.state === url.searchParams.get('state')));
      return Response.json({ runs: url.searchParams.has('cursor') ? rows.slice(1) : rows.slice(0, 1), membership_revision: 1, unavailable_kinds: fixture.unavailable_kinds, next_cursor: !url.searchParams.has('cursor') && rows.length > 1 ? 'next' : undefined });
    }
    if (url.pathname.startsWith('/api/v1/operations/runs/')) return Response.json({ ...run, related_status: 'getVisualAttachmentStatus', supported_actions: [] });
    return Response.json({}, { status: 404 });
  });
  const controller = new OperationsController(client); controllers.push(controller);
  const onNavigate = vi.fn();
  render(RunHistoryDialog, { client, controller, state: { ...defaultOperationsState(kind), ...filters }, onNavigate, onclose: vi.fn(), onConfigure: vi.fn() });
  return { onNavigate, controller, drift: () => { conflict = true; } };
}

describe('Past runs', () => {
  it('shows pending action feedback on the button until the request completes', async () => {
    const visualRun = fixture.runs.find(candidate => candidate.kind === 'visual_embedding')!;
    let resolveAction!: (response: Response) => void;
    const client = createAPIClient(async input => {
      const request = input as Request;
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/operations/status') return Response.json({ lanes: lanes(lane('visual_embedding', { supported_actions: ['visual_build'] })) });
      if (path === '/api/v1/operations/runs') return Response.json({ runs: [visualRun], membership_revision: 1, unavailable_kinds: [] });
      if (path === `/api/v1/operations/runs/${visualRun.id}`) return Response.json({ ...visualRun, supported_actions: ['visual_build'] });
      if (path === '/api/v1/multimodal/build' && request.method === 'POST') return new Promise<Response>(resolve => { resolveAction = resolve; });
      throw new Error(`Unexpected request: ${request.method} ${path}`);
    });
    const controller = new OperationsController(client); controllers.push(controller);
    const onAnnounce = vi.fn();
    render(RunHistoryDialog, { client, controller,
      state: { ...defaultOperationsState(), operationRunID: visualRun.id },
      onNavigate: vi.fn(), onclose: vi.fn(), onConfigure: vi.fn(), onAnnounce });
    await fireEvent.click(await screen.findByRole('button', { name: 'Build visual index' }));
    expect((await screen.findByRole<HTMLButtonElement>('button', { name: 'Build visual index…' })).disabled).toBe(true);
    expect(screen.queryByRole('status')).toBeNull();
    resolveAction(Response.json({}));
    await vi.waitFor(() => expect(onAnnounce).toHaveBeenCalledWith('Visual index build request completed; current operation state was refreshed.'));
    expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Build visual index' }).disabled).toBe(false);
  });

  it('keeps the same title while a deep-linked run loads and identifies its kind', async () => {
    let resolveDetail!: (response: Response) => void;
    const client = createAPIClient(async input => {
      const path = new URL((input as Request).url).pathname;
      if (path === '/api/v1/operations/status') return Response.json({ lanes: lanes() });
      if (path === '/api/v1/operations/runs') return Response.json({ runs: [], membership_revision: 1, unavailable_kinds: [] });
      if (path === `/api/v1/operations/runs/${run.id}`) return new Promise<Response>(resolve => { resolveDetail = resolve; });
      throw new Error(`Unexpected request: ${path}`);
    });
    const controller = new OperationsController(client); controllers.push(controller);
    render(RunHistoryDialog, { client, controller,
      state: { ...defaultOperationsState(), operationRunID: run.id },
      onNavigate: vi.fn(), onclose: vi.fn(), onConfigure: vi.fn() });
    await vi.waitFor(() => expect(resolveDetail).toBeDefined());
    expect(screen.getByRole('dialog', { name: 'Past runs' })).toBeTruthy();
    resolveDetail(Response.json({ ...run, supported_actions: [] }));
    const detail = await screen.findByRole('region', { name: 'Operation run detail' });
    expect(within(detail).getByText('Visual embedding')).toBeTruthy();
    expect(screen.getByRole('dialog', { name: 'Past runs' })).toBeTruthy();
  });

  it('reloads status counts when configuration changes', async () => {
    let configured = false;
    const client = createAPIClient(async input => {
      const path = new URL((input as Request).url).pathname;
      if (path === '/api/v1/operations/status') return Response.json({ lanes: lanes(lane('visual_embedding', { configured, related_status: 'getVisualAttachmentStatus' })) });
      if (path === '/api/v1/operations/runs') return Response.json({ runs: [], membership_revision: 1, unavailable_kinds: [] });
      if (path === '/api/v1/multimodal/status') return Response.json(visualStatus);
      throw new Error(`Unexpected request: ${path}`);
    });
    const controller = new OperationsController(client); controllers.push(controller);
    render(RunHistoryDialog, { client, controller,
      state: { ...defaultOperationsState(), operationStatus: 'getVisualAttachmentStatus' },
      onNavigate: vi.fn(), onclose: vi.fn(), onConfigure: vi.fn() });
    await screen.findByText('Visual attachments need configuration');
    configured = true;
    await controller.refreshStatus();
    await screen.findByText('0 of 10 attachments current');
    const dialog = screen.getByRole('dialog', { name: 'Visual attachment status' });
    expect(within(dialog).getAllByText('Visual attachment status', { exact: true })).toHaveLength(1);
    expect(screen.queryByText('Visual attachments need configuration')).toBeNull();
  });
  it('applies date bounds and exclusive kind or lane controls', async () => {
    vi.setSystemTime(new Date('2026-10-29T12:00:00Z'));
    const { controller } = setup('source_sync', { operationStartedFrom: '2026-10-10T00:00:00Z', operationStartedBefore: '2026-10-13T00:00:00Z' });
    await screen.findByRole('button', { name: 'Open Source sync run' });
    const applyURLState = vi.spyOn(controller, 'applyURLState');
    await fireEvent.click(screen.getByRole('button', { name: /2026-10-10/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Oct 11, 2026' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Oct 13, 2026' }));
    expect(applyURLState).toHaveBeenLastCalledWith(expect.objectContaining({ operationStartedFrom: '2026-10-11T00:00:00Z', operationStartedBefore: '2026-10-14T00:00:00Z' }), false);
    await fireEvent.keyDown(document, { key: 'Escape' });
    await chooseSelectOption(screen.getByRole('combobox', { name: /^Kind:/ }), 'All visual attachments');
    expect(applyURLState).toHaveBeenLastCalledWith(expect.objectContaining({ operationLane: 'visual_attachments', operationKind: '' }), false);
    await chooseSelectOption(screen.getByRole('combobox', { name: /^Kind:/ }), 'CardDAV sync');
    expect(applyURLState).toHaveBeenLastCalledWith(expect.objectContaining({ operationKind: 'carddav_sync', operationLane: '' }), false);
    await chooseSelectOption(screen.getByRole('combobox', { name: /^Kind:/ }), 'Source sync');
    await fireEvent.click(screen.getByRole('button', { name: /2026-10-11/ }));
    await fireEvent.click(screen.getByRole('radio', { name: 'Relative' }));
    await fireEvent.click(screen.getByRole('button', { name: 'All' }));
    expect(applyURLState).toHaveBeenLastCalledWith(expect.objectContaining({ operationStartedFrom: '', operationStartedBefore: '' }), false);
    await fireEvent.click(screen.getByRole('button', { name: '7d' }));
    expect(applyURLState).toHaveBeenLastCalledWith(expect.objectContaining({ operationStartedFrom: '2026-10-23T00:00:00Z', operationStartedBefore: '2026-10-30T00:00:00Z' }), false);
    await fireEvent.keyDown(document, { key: 'Escape' });
    await fireEvent.click(screen.getByRole('button', { name: 'Clear operation dates' }));
    expect(applyURLState).toHaveBeenLastCalledWith(expect.objectContaining({ operationStartedFrom: '', operationStartedBefore: '' }), false);
    await chooseSelectOption(screen.getByRole('combobox', { name: /^State:/ }), 'Paused');
    expect(applyURLState).toHaveBeenLastCalledWith(expect.objectContaining({ operationState: 'paused' }), false);
  });
  it('offers Restart when page membership changes', async () => {
    const { drift, controller } = setup();
    const restart = vi.spyOn(controller, 'restart');
    await screen.findByRole('button', { name: 'Show more' }); drift();
    await fireEvent.click(await screen.findByRole('button', { name: 'Show more' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Restart' }));
    expect(restart).toHaveBeenCalledOnce();
  });
  it('opens detail and routes related status', async () => {
    const { onNavigate } = setup();
    const row = await screen.findByRole('button', { name: 'Open Visual embedding run' });
    await fireEvent.click(row);
    await screen.findByRole('region', { name: 'Operation run detail' });
    await fireEvent.click(screen.getByRole('button', { name: 'Open Visual attachment status' }));
    expect(onNavigate).toHaveBeenCalledWith('getVisualAttachmentStatus');
  });
});

it('retries unreadable history and detail through Refresh', async () => {
  let failed = true;
  let detailFailed = true;
  const runs = [fixture.runs.find(run => run.kind === 'source_sync')!];
  const client = createAPIClient(async input => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
    if (path.endsWith('/status')) return Response.json({ lanes: lanes() });
    if (path.includes('/runs/')) return detailFailed ? Response.json({}, { status: 500 }) : Response.json({ ...runs[0], supported_actions: [] });
    return failed ? Response.json({}, { status: 500 }) : Response.json({ runs, membership_revision: 1, unavailable_kinds: [] });
  });
  const controller = new OperationsController(client); controllers.push(controller);
  const props = { client, controller, state: defaultOperationsState(), onNavigate: vi.fn(), onclose: vi.fn(), onConfigure: vi.fn() };
  render(RunHistoryDialog, props);
  await screen.findByText('Unable to load operation history.');
  failed = false;
  await fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
  await screen.findByRole('button', { name: 'Open Source sync run' });
  expect(screen.queryByText('Unable to load operation history.')).toBeNull();
  await fireEvent.click(screen.getByRole('button', { name: 'Open Source sync run' }));
  await screen.findByText('Unable to load operation detail.');
  detailFailed = false;
  await fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
  await screen.findByRole('region', { name: 'Operation run detail' });
  expect(screen.queryByText('Unable to load operation detail.')).toBeNull();
});

it('refreshes unchanged filters when the dialog opens', async () => {
  const client = createAPIClient(async input => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
    if (path === '/api/v1/operations/status') return Response.json({ lanes: lanes() });
    if (path === '/api/v1/operations/runs') return Response.json({ runs: [], membership_revision: 1, unavailable_kinds: [] });
    throw new Error(`Unexpected request: ${path}`);
  });
  const controller = new OperationsController(client); controllers.push(controller);
  const applyURLState = vi.spyOn(controller, 'applyURLState');
  const state = defaultOperationsState('source_sync');
  render(RunHistoryDialog, { client, controller, state, onNavigate: vi.fn(), onclose: vi.fn(), onConfigure: vi.fn() });
  await screen.findByText('No runs match.');
  expect(applyURLState).toHaveBeenCalledWith(state, true);
});

it('withholds an empty result while requested history is unavailable', async () => {
  let unavailable = true;
  const client = createAPIClient(async input => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
    if (path === '/api/v1/operations/status') return Response.json({ lanes: lanes() });
    if (path === '/api/v1/operations/runs') return Response.json({
      runs: [], membership_revision: 1,
      unavailable_kinds: unavailable ? [{ kind: 'visual_embedding', lane: 'visual_attachments', unavailable_code: 'visual_embedding_history_unavailable' }] : [],
    });
    throw new Error(`Unexpected request: ${path}`);
  });
  const controller = new OperationsController(client); controllers.push(controller);
  render(RunHistoryDialog, { client, controller,
    state: { ...defaultOperationsState(), operationLane: 'visual_attachments', operationState: 'failed' },
    onNavigate: vi.fn(), onclose: vi.fn(), onConfigure: vi.fn() });
  await screen.findByText('Visual embedding history is unavailable.');
  expect(screen.queryByText('No runs match.')).toBeNull();
  unavailable = false;
  await fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
  await screen.findByText('No runs match.');
  expect(screen.queryByText('Visual embedding history is unavailable.')).toBeNull();
});
