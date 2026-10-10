import { parseArchiveSelection } from './selection';

export function recordingSelection(messageID: number): string {
  return `recording:${messageID}`;
}

export function parseRecordingSelection(value: string | null): number | undefined {
  return parseArchiveSelection(value, 'recording:');
}
