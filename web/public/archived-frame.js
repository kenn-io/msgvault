// Bridge for archived mail frames (see web/src/lib/content/frame-document.ts).
//
// Served as a same-origin static asset because the daemon's shell CSP
// (script-src 'self') is inherited by sandboxed srcdoc frames, which blocks
// every inline script — including a hash-authorized one. The frame's own CSP
// admits exactly this URL and nothing else executable.
//
// The channel name and limits mirror ARCHIVED_CONTENT_CHANNEL,
// MAX_ARCHIVED_SCROLL_DELTA, and MAX_ARCHIVED_FRAME_HEIGHT in
// frame-document.ts; a unit test keeps them in sync.
(() => {
  'use strict';
  const root = document.documentElement;
  const nonce = root.dataset.bridgeNonce;
  const channel = 'msgvault-archived-content';
  const MAX_SCROLL_DELTA = 10000;
  const MAX_FRAME_HEIGHT = 65536;
  let origin;
  try {
    const declared = root.dataset.bridgeOrigin || '';
    const parsed = new URL(declared);
    if ((parsed.protocol === 'http:' || parsed.protocol === 'https:') && parsed.origin === declared) {
      origin = parsed.origin;
    }
  } catch {
    origin = undefined;
  }
  if (!nonce || !origin) return;

  const KEYS = new Set(['Escape', 'PageUp', 'PageDown', 'Home', 'End', 'ArrowUp', 'ArrowDown']);
  addEventListener('keydown', (event) => {
    if (KEYS.has(event.key)) {
      parent.postMessage({ channel, nonce, type: 'key', key: event.key }, origin);
    }
  });
  addEventListener('wheel', (event) => {
    if (Number.isFinite(event.deltaY) && Math.abs(event.deltaY) <= MAX_SCROLL_DELTA) {
      parent.postMessage({ channel, nonce, type: 'scroll', deltaY: event.deltaY }, origin);
    }
  }, { passive: true });

  // Height is measured from the root border box plus body overflow — never
  // documentElement.scrollHeight, whose viewport floor would ratchet the
  // frame: once the shell sizes it, a shorter document could never report a
  // smaller value. Horizontal overflow (wide layout tables) gets a scrollbar
  // allowance so the inner horizontal scrollbar never steals height and
  // forces a vertical one.
  const reportHeight = () => {
    const body = document.body;
    let height = Math.ceil(Math.max(
      root.getBoundingClientRect().height,
      body ? body.scrollHeight : 0
    ));
    if (root.scrollWidth > root.clientWidth) height += 18;
    height = Math.min(height, MAX_FRAME_HEIGHT);
    if (height > 0) parent.postMessage({ channel, nonce, type: 'height', height }, origin);
  };
  if (typeof ResizeObserver === 'function') {
    const observer = new ResizeObserver(reportHeight);
    observer.observe(root);
    if (document.body) observer.observe(document.body);
  }
  addEventListener('load', reportHeight);
  // Quote-chain details/summary toggles change content height without any
  // other signal in some engines; toggle does not bubble, so capture it.
  addEventListener('toggle', reportHeight, true);
  reportHeight();
})();
