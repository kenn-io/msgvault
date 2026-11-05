import { describe, expect, it } from 'vitest';
import { parseDelimited } from './text-preview';

describe('text previews', () => {
  it('parses quoted commas and embedded newlines', async () => {
    expect(await parseDelimited('Name,Note\n"Example, A","First\nSecond"', ',')).toEqual({ header: ['Name', 'Note'], rows: [['Example, A', 'First\nSecond']], truncated: false });
  });
  it('parses TSV', async () => {
    expect((await parseDelimited('Name\tCount\nExample\t42', '\t')).rows).toEqual([['Example', '42']]);
  });
  it('reports whether data rows were omitted', async () => {
    expect(await parseDelimited('Name\nOne\nTwo\nThree', ',', 2)).toEqual({ header: ['Name'], rows: [['One'], ['Two']], truncated: true });
    expect((await parseDelimited('Name\nOne\nTwo', ',', 2)).truncated).toBe(false);
  });
});
