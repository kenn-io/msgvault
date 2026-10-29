import { describe, expect, it } from 'vitest';

import type { OperationLaneStatus } from '../api/generated/models';
import { activeGeneration, embeddingStatus, lane, lanes, visualStatus } from '../../test/operations';
import { overviewRows } from './overview';

function row(rows: ReturnType<typeof overviewRows>, id: string) {
  const found = rows.find((candidate) => candidate.id === id);
  if (!found) throw new Error(`no row ${id}`);
  return found;
}

describe('overviewRows', () => {
  it('lists every background feature, off ones with a way to turn them on', () => {
    const rows = overviewRows(lanes(lane('message_embedding', { configured: false })), undefined, undefined);

    expect(rows.map((candidate) => candidate.name)).toEqual([
      'Source sync',
      'Message search',
      'People search',
      'Visual search',
      'People lookups',
      'Facts and briefs',
      'Contacts',
      'Document extraction',
      'Document search',
    ]);
    expect(row(rows, 'message_embedding')).toMatchObject({ chip: { text: 'Off' }, action: { kind: 'settings', target: { settingsCategory: 'search' } } });
    expect(row(rows, 'document_extraction').action).toMatchObject({ kind: 'host' });
  });

  it.each([
    { status: 'initializing' as const, available: true, built: false, chip: 'Initializing' },
    { status: 'initializing' as const, available: false, built: false, chip: 'Initializing' },
    { status: 'unavailable' as const, available: false, built: false, chip: 'Unavailable' },
    { status: 'unavailable' as const, available: true, built: false, chip: 'Unbuilt' },
    { status: 'ready' as const, available: false, built: true, chip: 'On' },
    { status: 'stale' as const, available: true, built: true, chip: 'Stale' },
  ])('gates the message build and chip on server coverage ($status)', ({ status, available, built, chip }) => {
    const rows = overviewRows(lanes(lane('message_embedding')), { ...embeddingStatus, active_generation: built ? activeGeneration : null }, undefined, { status, actions: available ? ['build_index'] : [] });
    expect(row(rows, 'message_embedding').action).toEqual(available ? { kind: 'start', id: 'messages', label: 'Build index' } : undefined);
    expect(row(rows, 'message_embedding').chip.text).toBe(chip);
  });

  it('offers the visual build the server advertises, with its consent line', () => {
    const rows = overviewRows(lanes(lane('visual_embedding', { supported_actions: ['visual_build'] })), undefined, undefined);

    expect(row(rows, 'visual_embedding').action).toMatchObject({ kind: 'start', id: 'visual_build', consent: expect.any(String) });
  });

  it('keeps disabled contacts Off and offers only the advertised sync action', () => {
    const rows = overviewRows(lanes(lane('carddav_sync', { configured: false, latest: { id: 'sync-1', kind: 'carddav_sync', lane: 'contacts', state: 'succeeded', started_at: '2026-01-01T00:00:00Z', counters: [] } })), undefined, undefined);

    expect(row(rows, 'carddav_sync').chip.text).toBe('Off');
    expect(row(rows, 'carddav_sync').action).toMatchObject({ kind: 'settings', target: { settingsCategory: 'carddav' } });
    expect(row(overviewRows(lanes(lane('carddav_sync')), undefined, undefined), 'carddav_sync').action).toBeUndefined();
    expect(row(overviewRows(lanes(lane('carddav_sync', { supported_actions: ['carddav_sync'] })), undefined, undefined), 'carddav_sync').action).toEqual({ kind: 'start', id: 'carddav_sync', label: 'Sync now' });
  });
});

it('shows index progress only while a run is active', () => {
  const embedding = { ...embeddingStatus, active_generation: activeGeneration, current: 2, pending: 8 };
  const visual = { ...visualStatus, current: 3 };
  for (const kind of ['message_embedding', 'visual_embedding'] as const) {
    const active: NonNullable<OperationLaneStatus['active']> = { id: 'run-1', kind, lane: kind === 'visual_embedding' ? 'visual_attachments' : 'messages', state: 'running', started_at: '2026-01-01T00:00:00Z', counters: [] };
    expect(row(overviewRows(lanes(lane(kind)), embedding, visual), kind).chip.text).toBe('On');
    expect(row(overviewRows(lanes(lane(kind, { active })), embedding, visual), kind).chip.text).toBe(`Building ${kind === 'message_embedding' ? 2 : 3} of 10`);
  }
});

it.each([['failed', 'Failed', 'danger'], ['partial', 'Partial', 'warning']] as const)('shows a %s latest run without offering Set up', (state, text, tone) => {
  const rows = overviewRows(lanes(lane('person_enrichment', { latest: { id: 'lookup-1', kind: 'person_enrichment', lane: 'person_facts', state, started_at: '2026-01-01T00:00:00Z', counters: [] } })), undefined, undefined);
  expect(row(rows, 'person_enrichment').chip).toEqual({ text, tone });
  expect(row(rows, 'person_enrichment').action).toBeUndefined();
});

it('shows unavailable history for a configured lane without a latest run', () => {
  const rows = overviewRows(lanes(lane('person_enrichment', { history_availability: 'unavailable' })), undefined, undefined);
  expect(row(rows, 'person_enrichment').chip).toEqual({ tone: 'muted', text: 'History unavailable' });
});
