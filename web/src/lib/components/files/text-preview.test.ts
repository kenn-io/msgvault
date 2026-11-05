import { describe, expect, it } from 'vitest';
import { decodeText, parseDelimited } from './text-preview';

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
