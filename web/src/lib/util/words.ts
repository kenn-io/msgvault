export function wordSeparator(index: number, length: number): string {
  if (index === 0) return '';
  if (index === length - 1) return length === 2 ? ' and ' : ', and ';
  return ', ';
}

export function joinWords(items: string[]): string {
  return items.map((item, index) => wordSeparator(index, items.length) + item).join('');
}
