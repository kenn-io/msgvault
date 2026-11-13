import { describe, expect, it } from 'vitest';
import { provenance, provenanceDetails, valueProvenance } from './history-format';
import { formatDateTime } from '../../util/format';

const year = new Date().getFullYear();
const at = `${year}-11-05T22:04:00`;

describe('provenance', () => {
  it('names who wrote a value in words', () => {
    expect(['user', 'extraction', 'enrichment', 'system', 'archive_observation', 'carddav_import'].map((source) => provenance(source, at).text))
      .toEqual(['You', 'From a message', 'From a lookup', 'msgvault', 'From your messages', 'From CardDAV contacts'].map((who) => `${who} · Nov 5`));
  });

  it('reads a short date, with the year only when it is not this year', () => {
    expect(provenance('extraction', '2025-11-05T22:04:00').text).toBe('From a message · Nov 5, 2025');
  });

  it('says since when the value became true on an earlier day than it was recorded', () => {
    expect(provenance('user', at, `${year}-11-01T09:00:00`).text).toBe('You · since Nov 1');
    expect(provenance('user', at, `${year}-11-05T09:00:00`).text).toBe('You · Nov 5');
  });

  it('names an act by you or msgvault and dates the act', () => {
    expect(provenance('system', at, undefined, { action: 'Merged' }).text).toBe('Merged by msgvault · Nov 5');
    expect(provenance('mcp', at, undefined, { action: 'Merged' }).text).toBe('Merged by an assistant · Nov 5');
    expect(provenance('cli', at, undefined, { action: 'Merged' }).text).toBe('Merged by you · Nov 5');
    expect(provenance('user', at, `${year}-11-01T09:00:00`, { action: 'Merged' }).text).toBe('Merged by you · Nov 5');
  });

  it.each([
    [{}, `Recorded Nov 5, ${year}, 10:04 PM\nValid from Nov 1, ${year}, 9:00 AM\nActor: you\nReference: ref-1\nConfidence: 80%`],
    [{ active_until: `${year}-11-06T09:00:00` }, `Recorded Nov 5, ${year}, 10:04 PM\nValid from Nov 1, ${year}, 9:00 AM\nValid until Nov 6, ${year}, 9:00 AM\nActor: you\nReference: ref-1\nConfidence: 80%`],
    [{ superseded_at: `${year}-11-07T09:00:00` }, `Recorded Nov 5, ${year}, 10:04 PM\nValid from Nov 1, ${year}, 9:00 AM\nReplaced Nov 7, ${year}, 9:00 AM\nActor: you\nReference: ref-1\nConfidence: 80%`],
    [{ active_until: `${year}-11-06T09:00:00`, superseded_at: `${year}-11-07T09:00:00` }, `Recorded Nov 5, ${year}, 10:04 PM\nValid from Nov 1, ${year}, 9:00 AM\nValid until Nov 6, ${year}, 9:00 AM\nReplaced Nov 7, ${year}, 9:00 AM\nActor: you\nReference: ref-1\nConfidence: 80%`],
  ])('keeps audit details in the tooltip while the line stays short: %j', (interval, title) => {
    const line = valueProvenance({ source: 'user', created_at: at, active_from: `${year}-11-01T09:00:00`, source_ref: 'ref-1', actor: 'user', confidence: 0.8, ...interval });
    expect(line.text).toBe('You · since Nov 1');
    expect(line.title).toBe(title);
  });
});

it('lists the recorded provenance details', () => {
  expect(provenanceDetails({ source: 'enrichment', actor: 'mcp', source_ref: 'lookup-7', confidence: 0, created_at: '2026-11-01T12:00:00' })).toEqual([
    ['Actor', 'an assistant'], ['Reference', 'lookup-7'], ['Confidence', '0%'], ['Recorded', formatDateTime('2026-11-01T12:00:00')]
  ]);
});

it('names generated decision references in details and History tooltips', () => {
  const generated = { source: 'enrichment', source_ref: 'person-fact-decision:123', created_at: at };
  expect(provenanceDetails(generated)).toContainEqual(['Reference', 'Fact decision']);
  const title = valueProvenance(generated).title;
  expect(title).toContain('Reference: Fact decision');
  expect(title).not.toContain('person-fact-decision:123');
});
