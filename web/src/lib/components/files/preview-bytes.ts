const IMAGE_SIGNATURES = {
  'image/png': (bytes: Uint8Array) => startsWith(bytes, [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
  'image/jpeg': (bytes: Uint8Array) => startsWith(bytes, [0xff, 0xd8, 0xff]),
  'image/gif': (bytes: Uint8Array) => startsWith(bytes, [...new TextEncoder().encode('GIF87a')]) ||
    startsWith(bytes, [...new TextEncoder().encode('GIF89a')]),
  'image/webp': (bytes: Uint8Array) => startsWith(bytes, [...new TextEncoder().encode('RIFF')]) &&
    bytes.length >= 12 && startsWith(bytes.subarray(8), [...new TextEncoder().encode('WEBP')])
} as const;

type SupportedImageMIME = keyof typeof IMAGE_SIGNATURES;
type SupportedThumbnailMIME = 'image/png' | 'image/jpeg';

const MAX_THUMBNAIL_DIMENSION = 4096;
const MAX_THUMBNAIL_PIXELS = 4 * 1024 * 1024;

function startsWith(bytes: Uint8Array, prefix: readonly number[]): boolean {
  return bytes.length >= prefix.length && prefix.every((value, index) => bytes[index] === value);
}

function normalizedMIME(value: string | null | undefined): string {
  return (value ?? '').split(';', 1)[0]!.trim().toLowerCase();
}

export function isSupportedImageMIME(value: string): value is SupportedImageMIME {
  return normalizedMIME(value) in IMAGE_SIGNATURES;
}

export function isSupportedThumbnailMIME(value: string): value is SupportedThumbnailMIME {
  const mime = normalizedMIME(value);
  return mime === 'image/png' || mime === 'image/jpeg';
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

// Automatic gallery thumbnails accept only static formats whose dimensions
// can be verified without invoking a browser decoder. GIF and WebP stay
// available through the explicit file viewer, but are not auto-decoded here
// because their animation frame counts cannot be bounded from a MIME header.
export function validatedThumbnailBlob(
  bytes: Uint8Array,
  metadataMIME: string,
  responseMIME: string | null
): Blob {
  const metadata = normalizedMIME(metadataMIME);
  if (!isSupportedThumbnailMIME(metadata)) {
    throw new Error('Thumbnail preview was rejected because its format cannot be decoded safely.');
  }
  const blob = validatedImageBlob(bytes, metadata, responseMIME);
  const dimensions = metadata === 'image/png' ? pngDimensions(bytes) : jpegDimensions(bytes);
  if (
    dimensions.width <= 0 || dimensions.height <= 0 ||
    dimensions.width > MAX_THUMBNAIL_DIMENSION || dimensions.height > MAX_THUMBNAIL_DIMENSION ||
    dimensions.width * dimensions.height > MAX_THUMBNAIL_PIXELS
  ) {
    throw new Error('Thumbnail preview was rejected because its decoded dimensions are too large.');
  }
  return blob;
}

function pngDimensions(bytes: Uint8Array): { width: number; height: number } {
  let offset = 8;
  let dimensions: { width: number; height: number } | undefined;
  let foundEnd = false;
  while (offset + 12 <= bytes.length) {
    const length = readU32(bytes, offset);
    if (length > bytes.length - offset - 12) break;
    const type = String.fromCharCode(...bytes.subarray(offset + 4, offset + 8));
    const dataOffset = offset + 8;
    if (!dimensions) {
      if (type !== 'IHDR' || length !== 13) break;
      dimensions = { width: readU32(bytes, dataOffset), height: readU32(bytes, dataOffset + 4) };
    }
    if (type === 'acTL') {
      throw new Error('Thumbnail preview was rejected because animated images are not decoded automatically.');
    }
    offset += 12 + length;
    if (type === 'IEND') {
      foundEnd = length === 0;
      break;
    }
  }
  if (!dimensions || !foundEnd) {
    throw new Error('Thumbnail preview was rejected because its PNG structure is invalid.');
  }
  return dimensions;
}

function jpegDimensions(bytes: Uint8Array): { width: number; height: number } {
  let offset = 2;
  while (offset < bytes.length) {
    if (bytes[offset] !== 0xff) break;
    while (offset < bytes.length && bytes[offset] === 0xff) offset += 1;
    if (offset >= bytes.length) break;
    const marker = bytes[offset++]!;
    if (marker === 0xd9 || marker === 0xda) break;
    if (marker === 0x01 || (marker >= 0xd0 && marker <= 0xd7)) continue;
    if (offset + 2 > bytes.length) break;
    const length = readU16(bytes, offset);
    if (length < 2 || length > bytes.length - offset) break;
    if (isJPEGStartOfFrame(marker)) {
      if (length < 7) break;
      return { height: readU16(bytes, offset + 3), width: readU16(bytes, offset + 5) };
    }
    offset += length;
  }
  throw new Error('Thumbnail preview was rejected because its JPEG dimensions are invalid.');
}

function isJPEGStartOfFrame(marker: number): boolean {
  return marker >= 0xc0 && marker <= 0xcf && ![0xc4, 0xc8, 0xcc].includes(marker);
}

function readU16(bytes: Uint8Array, offset: number): number {
  return (bytes[offset]! << 8) | bytes[offset + 1]!;
}

function readU32(bytes: Uint8Array, offset: number): number {
  return (
    bytes[offset]! * 0x1000000 +
    (bytes[offset + 1]! << 16) +
    (bytes[offset + 2]! << 8) +
    bytes[offset + 3]!
  );
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
