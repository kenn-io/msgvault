import type { ChipTone } from '@kenn-io/kit-ui';

import type { EmbeddingStatus, OperationLaneStatus, Status } from '../api/generated/models';
import { OPERATION_SETUP, type OperationSetup } from '../operations/labels';

/** What the one button on a row does. */
export type OverviewAction =
  | OperationSetup
  | { kind: 'start'; id: 'messages' | 'visual_build' | 'visual_resume' | 'contacts'; label: string; consent?: string }
  | { kind: 'sources' };

export interface OverviewRow {
  id: string;
  name: string;
  chip: { tone: ChipTone; text: string };
  action?: OverviewAction;
}

const BUILD_VISUAL_CONSENT = 'Sends attachments to your search provider.';

type Kind = OperationLaneStatus['kind'];

function lane(lanes: readonly OperationLaneStatus[], kind: Kind): OperationLaneStatus | undefined {
  return lanes.find((candidate) => candidate.kind === kind);
}

function failed(found: OperationLaneStatus | undefined): boolean {
  const state = found?.latest?.state;
  return !found?.active && (state === 'failed' || state === 'partial');
}

function count(value: unknown): number {
  return Number(value) || 0;
}

/**
 * One row per background feature, from the operation lanes plus the message and
 * visual index counts that Settings already loads.
 */
export function overviewRows(
  lanes: readonly OperationLaneStatus[],
  embedding: EmbeddingStatus | undefined,
  visual: Status | undefined,
): OverviewRow[] {
  // Contacts connections sync without the legacy single-account switch, so a
  // contacts lane that has run counts as on.
  const on = (kind: Kind): boolean => {
    const found = lane(lanes, kind);
    return Boolean(found?.configured || (kind === 'carddav_sync' && found?.latest));
  };
  const chip = (kind: Kind, building?: string): OverviewRow['chip'] => {
    const found = lane(lanes, kind);
    if (!found || found.unavailable_code) return { tone: 'muted', text: 'Unavailable' };
    if (found.active) return { tone: 'info', text: found.active.state === 'queued' ? 'Queued' : 'Running' };
    if (!on(kind)) return { tone: 'muted', text: 'Off' };
    if (failed(lane(lanes, kind))) return { tone: 'danger', text: 'Failed' };
    if (building) return { tone: 'info', text: building };
    return { tone: 'success', text: 'On' };
  };
  const rows: OverviewRow[] = [];
  const setup = (kind: Kind): OverviewAction | undefined => kind === 'carddav_sync'
    ? { kind: 'settings', target: { settingsCategory: 'carddav', settingsAuthority: '' } }
    : OPERATION_SETUP[kind];

  const messagesBuilt = Boolean(embedding?.active_generation);
  const messageProgress =
    embedding && messagesBuilt && count(embedding.pending) > 0
      ? `Building ${count(embedding.current).toLocaleString()} of ${count(embedding.eligible).toLocaleString()}`
      : undefined;
  const needsIndex = on('message_embedding') && embedding !== undefined && !messagesBuilt;
  rows.push({
    id: 'message_embedding',
    name: 'Message search',
    chip: needsIndex && !lane(lanes, 'message_embedding')?.active && !failed(lane(lanes, 'message_embedding')) ? { tone: 'muted', text: 'Unbuilt' } : chip('message_embedding', messageProgress),
    action: needsIndex && !lane(lanes, 'message_embedding')?.active
      ? { kind: 'start', id: 'messages', label: 'Build index' }
      : on('message_embedding')
        ? undefined
        : setup('message_embedding'),
  });

  rows.push({
    id: 'person_embedding',
    name: 'People search',
    chip: chip('person_embedding'),
    action: on('person_embedding') ? undefined : setup('person_embedding'),
  });

  const visualLane = lane(lanes, 'visual_embedding');
  const visualCurrent = count(visual?.current);
  const visualEligible = count(visual?.eligible);
  const visualProgress =
    visual && visualCurrent < visualEligible
      ? `Building ${visualCurrent.toLocaleString()} of ${visualEligible.toLocaleString()}`
      : undefined;
  const visualActions = visualLane?.supported_actions ?? [];
  rows.push({
    id: 'visual_embedding',
    name: 'Visual search',
    chip: chip('visual_embedding', visualProgress),
    action: !on('visual_embedding')
      ? setup('visual_embedding')
      : visualActions.includes('visual_build')
        ? { kind: 'start', id: 'visual_build', label: 'Build visual index', consent: BUILD_VISUAL_CONSENT }
        : visualActions.includes('visual_resume')
          ? { kind: 'start', id: 'visual_resume', label: 'Resume visual index' }
          : undefined,
  });

  rows.push({
    id: 'person_enrichment',
    name: 'People lookups',
    chip: chip('person_enrichment'),
    action: on('person_enrichment') ? undefined : setup('person_enrichment'),
  });

  rows.push({
    id: 'person_sweep',
    name: 'Facts and briefs',
    chip: chip('person_sweep'),
    action: on('person_sweep') ? undefined : setup('person_sweep'),
  });

  rows.push({
    id: 'carddav_sync',
    name: 'Contacts',
    chip: chip('carddav_sync'),
    action: !on('carddav_sync')
      ? setup('carddav_sync')
      : lane(lanes, 'carddav_sync')?.supported_actions.includes('carddav_sync')
        ? { kind: 'start', id: 'contacts', label: 'Sync now' }
        : undefined,
  });

  rows.push({
    id: 'document_extraction',
    name: 'Document extraction',
    chip: chip('document_extraction'),
    action: on('document_extraction')
      ? undefined
      : setup('document_extraction'),
  });
  rows.push({
    id: 'document_embedding',
    name: 'Document search',
    chip: chip('document_embedding'),
    action: on('document_embedding')
      ? undefined
      : setup('document_embedding'),
  });

  rows.unshift({ id: 'source_sync', name: 'Source sync', chip: chip('source_sync'), action: { kind: 'sources' } });
  return rows.map((row) => {
    const action = setup(row.id as Kind);
    return !row.action && row.chip.text === 'Failed' && action?.kind === 'settings' ? { ...row, action } : row;
  });
}
