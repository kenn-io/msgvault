import { describe, expect, it } from 'vitest';
import { decodeText, parseDelimited } from './text-preview';

describe('text previews', () => {
  it('parses quoted commas and embedded newlines', async () => {
    expect(await parseDelimited('Name,Note\n"Example, A","First\nSecond"', ',')).toEqual({ header: ['Name', 'Note'], rows: [['Example, A', 'First\nSecond']], truncated: false, hasErrors: false });
  });
  it('parses TSV', async () => {
    expect((await parseDelimited('Name\tCount\nExample\t42', '\t')).rows).toEqual([['Example', '42']]);
  });
  it('reports whether data rows were omitted', async () => {
    expect(await parseDelimited('Name\nOne\nTwo\nThree', ',', 2)).toEqual({ header: ['Name'], rows: [['One'], ['Two']], truncated: true, hasErrors: false });
    expect((await parseDelimited('Name\nOne\nTwo', ',', 2)).truncated).toBe(false);
  });
});

it.each([
  [[0xff, 0xfe, 0x48, 0, 0xe9, 0], 'text/plain; charset=utf-8', 'Hé'],
  [[0xfe, 0xff, 0, 0x48, 0, 0xe9], 'text/plain; charset=windows-1252', 'Hé'],
  [[0xef, 0xbb, 0xbf, 0xc3, 0xa9], 'text/plain; charset=windows-1252', 'é'],
  [[0x43, 0x61, 0x66, 0xe9], 'text/plain; charset="windows-1252"', 'Café'],
  [[0x48, 0, 0x69, 0], 'text/plain; charset=utf-16le', 'Hi'],
  [[0xc3, 0xa9], 'text/plain', 'é'],
  [[0xc3, 0xa9], 'text/plain; charset=unknown', 'é'],
])('decodes BOM or declared encoding before the UTF-8 fallback (%s)', (bytes, mime, want) => {
  expect(decodeText(new Uint8Array(bytes), mime)).toBe(want);
});
it('omits only an unfinished character when a prefix cuts UTF-8', () => {
  expect(decodeText(new Uint8Array([0x41, 0xe2, 0x82]), 'text/plain', true)).toBe('A');
  expect(decodeText(new Uint8Array([0x41, 0xe2, 0x82, 0xac]), 'text/plain', true)).toBe('A€');
});

it('detects semicolon CSV columns', async () => {
  expect((await parseDelimited('Name;Amount\nExample;42\nOther;73')).rows).toEqual([['Example', '42'], ['Other', '73']]);
});
it.each([
  'Name,Note,Amount\nComplete,Ready,99\nPartial,Long note,12',
  'Name,Note,Amount\nComplete,Ready,99\nPartial,"Long\nnote\n',
])('drops the unfinished record in a byte prefix', async (text) => {
  expect((await parseDelimited(text, ',', 200, true)).rows).toEqual([['Complete', 'Ready', '99']]);
});
it('retains the final record when the byte prefix ends at a record terminator', async () => {
  expect((await parseDelimited('Name,Amount\nComplete,99\n', ',', 200, true)).rows).toEqual([['Complete', '99']]);
});
it('reports malformed quoted fields', async () => {
  expect((await parseDelimited('Name,Amount\nExample,"42\nOther,73', ',')).hasErrors).toBe(true);
});

it.each(['Name\nOne\nTwo', 'Name,Amount\nExample,42,extra\nOther,73'])('keeps readable rows without an unreadable warning', async (text) => {
  expect((await parseDelimited(text)).hasErrors).toBe(false);
});

it('pads ragged headers and rows to the widest visible record', async () => {
  expect(await parseDelimited('Name,Amount\nExample,42,extra\nOther', ',')).toEqual({
    header: ['Name', 'Amount', ''], rows: [['Example', '42', 'extra'], ['Other', '', '']], truncated: false, hasErrors: false
  });
});

it('bounds ragged CSV expansion to 100 columns and 200 rows', async () => {
  const text = Array(20_000).fill('Header').join(',') + '\n' + Array(200).fill('Value').join('\n');
  const result = await parseDelimited(text, ',');
  expect(result.header).toHaveLength(100);
  expect(result.rows).toHaveLength(200);
  expect(result.rows.every((row) => row.length === 100 && row[0] === 'Value')).toBe(true);
  expect(result.rows.reduce((count, row) => count + row.length, 0)).toBeLessThanOrEqual(20_000);
});
