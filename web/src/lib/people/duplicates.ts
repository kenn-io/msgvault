import type { IdentityMatchCandidate } from '../api/generated/models';
/** A match joins two endpoints of different kinds, so ids are only unique with their kind: "participant:12". */
export function endpointKey(candidate: IdentityMatchCandidate, side: 'left' | 'right'): string {
  return side === 'left' ? `${candidate.left_kind}:${candidate.left_id}` : `${candidate.right_kind}:${candidate.right_id}`;
}

// Endpoints with nothing to look up are named for what they are, with the value they matched on.
export const ENDPOINT_NAMES: Readonly<Record<string, string>> = {
  carddav_resource: 'Address-book contact',
  contact_point: 'Contact detail',
  observation: 'Seen in messages',
};

export interface EndpointSummary {
  id: number;
  name: string;
  address: string;
  lastAt: string | null;
  count?: number;
}
