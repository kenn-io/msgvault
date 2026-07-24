import {
  ARCHIVED_CONTENT_CHANNEL,
  MAX_ARCHIVED_FRAME_HEIGHT,
  MAX_ARCHIVED_SCROLL_DELTA
} from '../../content/frame-document';

const FRAME_KEYS = new Set(['Escape', 'PageUp', 'PageDown', 'Home', 'End', 'ArrowUp', 'ArrowDown']);

interface ArchivedContentHandlerOptions {
  frameWindow: () => Window | null;
  nonce: () => string;
  onKey: (key: string) => void;
  onScroll: (deltaY: number) => void;
  onHeight: (height: number) => void;
}

function exactKeys(value: Record<string, unknown>, expected: string[]): boolean {
  const keys = Object.keys(value).sort();
  return keys.length === expected.length && expected.sort().every((key, index) => keys[index] === key);
}

export function createArchivedContentMessageHandler(
  options: ArchivedContentHandlerOptions
): (event: MessageEvent) => void {
  return (event) => {
    if (event.source !== options.frameWindow() || event.origin !== 'null') return;
    if (typeof event.data !== 'object' || event.data === null || Array.isArray(event.data)) return;
    const data = event.data as Record<string, unknown>;
    if (data.channel !== ARCHIVED_CONTENT_CHANNEL || data.nonce !== options.nonce()) return;
    if (data.type === 'key') {
      if (!exactKeys(data, ['channel', 'nonce', 'type', 'key'])) return;
      if (typeof data.key !== 'string' || !FRAME_KEYS.has(data.key)) return;
      options.onKey(data.key);
      return;
    }
    if (data.type === 'scroll') {
      if (!exactKeys(data, ['channel', 'nonce', 'type', 'deltaY'])) return;
      if (typeof data.deltaY !== 'number' || !Number.isFinite(data.deltaY) ||
        Math.abs(data.deltaY) > MAX_ARCHIVED_SCROLL_DELTA) return;
      options.onScroll(data.deltaY);
      return;
    }
    if (data.type === 'height') {
      if (!exactKeys(data, ['channel', 'nonce', 'type', 'height'])) return;
      if (typeof data.height !== 'number' || !Number.isFinite(data.height) ||
        data.height <= 0 || data.height > MAX_ARCHIVED_FRAME_HEIGHT) return;
      options.onHeight(data.height);
    }
  };
}

export function createFrameNonce(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(32));
  let binary = '';
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replaceAll('+', '-').replaceAll('/', '_').replace(/=+$/, '');
}
