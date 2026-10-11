import { parseArchiveSelection } from '../archive/selection';

export function parseAttachmentSelection(value: string | null): number | undefined {
  return parseArchiveSelection(value, 'attachment:');
}
