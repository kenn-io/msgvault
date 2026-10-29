import type { SearchCoverageValue } from '../search/modes';
import type { ChipTone } from '@kenn-io/kit-ui';

import type { EmbeddingStatus, OperationLaneStatus, Status } from '../api/generated/models';
import { OPERATION_SETUP, operationStateChip, type OperationSetup } from '../operations/labels';

/** What the one button on a row does. */
export type OverviewAction =
  | OperationSetup
  | { kind: 'start'; id: 'messages' | 'visual_build' | 'visual_resume' | 'carddav_sync'; label: string; consent?: string }
  | { kind: 'sources' };

export interface OverviewRow {
  id: string;
  name: string;
  chip: { tone: ChipTone; text: string };
  action?: OverviewAction;
}

const BUILD_VISUAL_CONSENT = 'Sends attachments to your search provider.';
const COVERAGE_CHIPS: Partial<Record<SearchCoverageValue['status'], OverviewRow['chip']>> = {
  stale: { tone: 'warning', text: 'Stale' },
  initializing: { tone: 'info', text: 'Initializing' },
  unavailable: { tone: 'warning', text: 'Unavailable' },
};

type Kind = OperationLaneStatus['kind'];

function lane(lanes: readonly OperationLaneStatus[], kind: Kind): OperationLaneStatus | undefined {
  return lanes.find((candidate) => candidate.kind === kind);
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
  coverage?: Pick<SearchCoverageValue, 'status' | 'actions'>,
): OverviewRow[] {
  const on = (kind: Kind): boolean => Boolean(lane(lanes, kind)?.configured);
  const chip = (kind: Kind, building?: string, idle?: OverviewRow['chip']): OverviewRow['chip'] => {
    const found = lane(lanes, kind);
    if (found?.active) {
      const state = operationStateChip(found.active.state);
      return { tone: state.tone, text: found.active.state === 'running' && building ? building : state.label };
    }
    if (!on(kind)) return { tone: 'muted', text: 'Off' };
    if (found?.latest && found.latest.state !== 'succeeded') {
      const state = operationStateChip(found.latest.state);
      return { tone: state.tone, text: state.label };
    }
    if (idle) return idle;
    return found?.history_availability === 'available' ? { tone: 'success', text: 'On' } : { tone: 'muted', text: 'History unavailable' };
  };
  const rows: OverviewRow[] = [];
  const setup = (kind: Kind): OverviewAction | undefined => kind === 'carddav_sync'
    ? { kind: 'settings', target: { settingsCategory: 'carddav', settingsAuthority: '' } }
    : OPERATION_SETUP[kind];

  const configuredRow = (id: Kind, name: string): OverviewRow => ({ id, name, chip: chip(id), action: on(id) ? undefined : setup(id) });

  const messagesBuilt = Boolean(embedding?.active_generation);
  const messageProgress =
    embedding && messagesBuilt && count(embedding.pending) > 0
      ? `Building ${count(embedding.current).toLocaleString()} of ${count(embedding.eligible).toLocaleString()}`
      : undefined;
  const buildAvailable = Boolean(coverage?.actions.includes('build_index'));
  rows.push({
    id: 'message_embedding',
    name: 'Message search',
    chip: chip('message_embedding', messageProgress, coverage?.status === 'unavailable' && buildAvailable ? { tone: 'muted', text: 'Unbuilt' } : coverage && COVERAGE_CHIPS[coverage.status]),
    action: buildAvailable
      ? { kind: 'start', id: 'messages', label: 'Build index' }
      : on('message_embedding')
        ? undefined
        : setup('message_embedding'),
  });

  rows.push(configuredRow('person_embedding', 'People search'));

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

  rows.push(configuredRow('person_enrichment', 'People lookups'));

  rows.push(configuredRow('person_sweep', 'Facts and briefs'));

  rows.push({
    id: 'carddav_sync',
    name: 'Contacts',
    chip: chip('carddav_sync'),
    action: !on('carddav_sync')
      ? setup('carddav_sync')
      : lane(lanes, 'carddav_sync')?.supported_actions.includes('carddav_sync')
        ? { kind: 'start', id: 'carddav_sync', label: 'Sync now' }
        : undefined,
  });

  rows.push(configuredRow('document_extraction', 'Document extraction'));
  rows.push(configuredRow('document_embedding', 'Document search'));

  rows.unshift({ id: 'source_sync', name: 'Source sync', chip: chip('source_sync'), action: { kind: 'sources' } });
  return rows;
}
