export function recordingSelection(messageID: number): string {
  return `recording:${messageID}`;
}

export function parseRecordingSelection(value: string | null): number | undefined {
  const match = /^recording:([1-9][0-9]*)$/.exec(value ?? '');
  if (!match) return undefined;
  const messageID = Number(match[1]);
  return Number.isSafeInteger(messageID) ? messageID : undefined;
}
