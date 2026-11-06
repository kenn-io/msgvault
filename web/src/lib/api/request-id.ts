export function createBrowserRequestID(): string {
  return globalThis.crypto.getRandomValues(new Uint32Array(4)).join('-');
}
