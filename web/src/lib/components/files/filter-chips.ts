import { FILE_FAMILY_LABELS } from '../../explore/labels';
import type { FileMIMEFamily } from '../../explore/models';

export function fileFilterChips(
  filename: string,
  families: FileMIMEFamily[],
  onFilename: (query: string) => void,
  onTypes: (families: FileMIMEFamily[]) => void,
) {
  return [
    ...(filename ? [{ key: 'filename', label: `Filename: “${filename}”`, removeLabel: 'Remove filename filter', onRemove: () => onFilename('') }] : []),
    ...(families.length ? [{ key: 'type', label: `Type: ${families.map((family) => FILE_FAMILY_LABELS[family]).join(', ')}`, removeLabel: 'Remove type filter', onRemove: () => onTypes([]) }] : []),
  ];
}
