const IMAGE_SIGNATURES = {
  'image/png': (bytes: Uint8Array) => startsWith(bytes, [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
  'image/jpeg': (bytes: Uint8Array) => startsWith(bytes, [0xff, 0xd8, 0xff]),
  'image/gif': (bytes: Uint8Array) => startsWith(bytes, [...new TextEncoder().encode('GIF87a')]) ||
    startsWith(bytes, [...new TextEncoder().encode('GIF89a')]),
  'image/webp': (bytes: Uint8Array) => startsWith(bytes, [...new TextEncoder().encode('RIFF')]) &&
    bytes.length >= 12 && startsWith(bytes.subarray(8), [...new TextEncoder().encode('WEBP')])
} as const;

type SupportedImageMIME = keyof typeof IMAGE_SIGNATURES;

function startsWith(bytes: Uint8Array, prefix: readonly number[]): boolean {
  return bytes.length >= prefix.length && prefix.every((value, index) => bytes[index] === value);
}

function normalizedMIME(value: string | null | undefined): string {
  return (value ?? '').split(';', 1)[0]!.trim().toLowerCase();
}

export function isSupportedImageMIME(value: string): value is SupportedImageMIME {
  return normalizedMIME(value) in IMAGE_SIGNATURES;
}

export function validatedImageBlob(
  bytes: Uint8Array,
  metadataMIME: string,
  responseMIME: string | null
): Blob {
  const metadata = normalizedMIME(metadataMIME);
  const response = normalizedMIME(responseMIME);
  if (!isSupportedImageMIME(metadata) || response !== metadata || !IMAGE_SIGNATURES[metadata](bytes)) {
    throw new Error('Image preview was rejected because its MIME type and file signature do not agree.');
  }
  return new Blob([new Uint8Array(bytes).buffer], { type: metadata });
}

export async function readBoundedStream(
  stream: ReadableStream<Uint8Array> | null,
  headers: Headers,
  signal: AbortSignal,
  maxBytes: number
): Promise<Uint8Array> {
  const declared = headers.get('Content-Length');
  if (declared !== null) {
    const length = Number(declared);
    if (Number.isFinite(length) && length > maxBytes) {
      if (stream) {
        try {
          await stream.cancel();
        } catch {
          // The stable size error is authoritative even when transport cleanup fails.
        }
      }
      throw new Error('File exceeds the preview byte limit.');
    }
  }
  if (!stream) return new Uint8Array();
  const reader = stream.getReader();
  const cancel = (): void => { void reader.cancel(); };
  signal.addEventListener('abort', cancel, { once: true });
  const chunks: Uint8Array[] = [];
  let length = 0;
  try {
    while (true) {
      if (signal.aborted) throw new DOMException('Preview cancelled', 'AbortError');
      const { done, value } = await reader.read();
      if (done) break;
      length += value.byteLength;
      if (length > maxBytes) {
        await reader.cancel();
        throw new Error('File exceeds the preview byte limit.');
      }
      chunks.push(value);
    }
  } finally {
    signal.removeEventListener('abort', cancel);
    reader.releaseLock();
  }
  const bytes = new Uint8Array(length);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return bytes;
}
