import { describe, expect, it } from 'vitest';
import { provenance, valueProvenance } from './history-format';

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
    {},
    { active_until: `${year}-11-06T09:00:00` },
    { superseded_at: `${year}-11-07T09:00:00` },
    { active_until: `${year}-11-06T09:00:00`, superseded_at: `${year}-11-07T09:00:00` },
  ])('keeps audit details in the tooltip while the line stays short: %j', interval => {
    const line = valueProvenance({ source: 'user', created_at: at, active_from: `${year}-11-01T09:00:00`, source_ref: 'ref-1', actor: 'user', confidence: 0.8, ...interval });
    expect(line.text).toBe('You · since Nov 1');
    expect(line.title.split('\n').map(entry => entry.startsWith('Valid') ? entry.split(' ').slice(0, 2).join(' ') : entry.split(' ')[0])).toEqual([
      'Recorded', 'Valid from', ...('active_until' in interval ? ['Valid until'] : []), ...('superseded_at' in interval ? ['Replaced'] : []), 'Actor:', 'Reference:', 'Confidence:',
    ]);
    if ('active_until' in interval) expect(line.title).toMatch(/Valid until Nov 6, \d{4}/);
    if ('superseded_at' in interval) expect(line.title).toMatch(/Replaced Nov 7, \d{4}/);
    expect(line.title).toContain('Actor: you\nReference: ref-1\nConfidence: 0.8');
  });
});
