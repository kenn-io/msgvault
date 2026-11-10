import { describe, expect, it } from 'vitest';
import { decodeText, parseDelimited } from './text-preview';

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

describe('delimited previews', () => {
  it('keeps quoted empty values and blank lines but not a trailing terminator', async () => {
    for (const text of ['value\nfirst\n""\nthird', 'value\nfirst\n""\nthird\n', 'value\r\nfirst\r\n""\r\nthird\r\n', 'value\rfirst\r""\rthird\r']) {
      expect((await parseDelimited(text, ',')).rows).toEqual([['first'], [''], ['third']]);
    }
    expect((await parseDelimited('value\nfirst\n""\n', ',')).rows).toEqual([['first'], ['']]);
    expect((await parseDelimited('value\nfirst\n""\n', ',', 200, true)).rows).toEqual([['first'], ['']]);
    expect((await parseDelimited('value\nfirst\n""', ',', 200, true)).rows).toEqual([['first']]);
    expect((await parseDelimited('value\nfirst\n\nthird\n', ',')).rows).toEqual([['first'], [''], ['third']]);
    expect((await parseDelimited('h\n1\n', ',', 1)).truncated).toBe(false);
  });
  it('still guesses delimiters when the text ends with a terminator', async () => {
    expect((await parseDelimited('a;b\n1;2\n')).rows).toEqual([['1', '2']]);
    expect((await parseDelimited('a|b\n\n1|2\n')).rows).toEqual([[''], ['1', '2']]);
  });
  it('reports whether data rows were omitted', async () => {
    expect(await parseDelimited('Name\nOne\nTwo\nThree', ',', 2)).toEqual({ header: ['Name'], rows: [['One'], ['Two']], truncated: true, cellLimit: false, columnsTruncated: false, hasErrors: false });
    expect((await parseDelimited('Name\nOne\nTwo', ',', 2)).truncated).toBe(false);
  });
  it('keeps complete records at byte boundaries and drops unfinished records', async () => {
    for (const text of ['Name,Note,Amount\nComplete,Ready,99\nPartial,Long note,12', 'Name,Note,Amount\nComplete,Ready,99\nPartial,"Long\nnote\n']) {
      expect((await parseDelimited(text, ',', 200, true)).rows).toEqual([['Complete', 'Ready', '99']]);
    }
    expect((await parseDelimited('Name,Amount\nComplete,99\n', ',', 200, true)).rows).toEqual([['Complete', '99']]);
    expect((await parseDelimited('Name,Note\r\nComplete,Ready\r', ',', 200, true)).rows).toEqual([['Complete', 'Ready']]);
    const quoted = await parseDelimited('"Name\ncontinued",Note\r\nA,B\r', ',', 200, true);
    expect(quoted.header).toEqual(['Name\ncontinued', 'Note']);
    expect(quoted.rows).toEqual([['A', 'B']]);
  });
  it('removes the final CRLF terminator after LF records', async () => {
    expect((await parseDelimited('Name,Amount\nExample,42\r', ',')).rows).toEqual([['Example', '42']]);
    expect((await parseDelimited('Name,Amount\nExample,42\r\n', ',')).rows).toEqual([['Example', '42']]);
    expect((await parseDelimited('Name,Amount\nExample,42\r\nOther,73\n', ',')).rows).toEqual([['Example', '42'], ['Other', '73']]);
    expect((await parseDelimited('Name,Note\nExample,"line\r\ninside"\nOther,done\n', ',')).rows).toEqual([['Example', 'line\r\ninside'], ['Other', 'done']]);
    expect((await parseDelimited('name\x1enote\nalpha\x1e"one\r\ntwo"\n')).rows).toEqual([['alpha', 'one\r\ntwo']]);
  });
  it('reports malformed quotes without blaming readable or truncated rows', async () => {
    expect((await parseDelimited('Name,Amount\nExample,"42\nOther,73', ',')).hasErrors).toBe(true);
    const result = await parseDelimited('Name,Note\nComplete,Ready\nPartial,"unfinished', ',', 200, true);
    expect(result.rows).toEqual([['Complete', 'Ready']]);
    expect(result.hasErrors).toBe(false);
  });
  it('pads headers, keeps short rows sparse, and retains wide headers', async () => {
    expect(await parseDelimited('Name,Amount\nExample,42,extra\nOther', ',')).toEqual({
      header: ['Name', 'Amount', ''], rows: [['Example', '42', 'extra'], ['Other']], truncated: false, cellLimit: false, columnsTruncated: false, hasErrors: false
    });
    const result = await parseDelimited(Array(101).fill('Header').join(',') + '\nValue', ',');
    expect(result.header).toHaveLength(101);
    expect(result.rows[0]).toHaveLength(1);
  });
  it('limits rendered cells while retaining complete wide rows', async () => {
    const row = Array(1000).fill('x').join(',');
    const result = await parseDelimited(Array(22).fill(row).join('\n'), ',');
    expect(result.header).toHaveLength(1000);
    expect(result.rows).toHaveLength(19);
    expect(result.rows[0]).toHaveLength(1000);
    expect(result.cellLimit).toBe(true);
    expect(result.columnsTruncated).toBe(false);
    const wider = await parseDelimited(Array(20001).fill('x').join(',') + '\nvalue', ',');
    expect(wider.header).toHaveLength(20000);
    expect(wider.columnsTruncated).toBe(true);
  });
});
