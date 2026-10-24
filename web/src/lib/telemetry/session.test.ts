import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { createSessionAwareAPIClient } from '../api/client';
import { startSessionReporting } from './session';
import { startTelemetryReporting } from './actions';

let now = 0;
let hidden = false;
let requests: Request[];
let stop: () => void;

beforeEach(() => {
  vi.useFakeTimers();
  now = 0;
  hidden = false;
  requests = [];
  vi.spyOn(performance, 'now').mockImplementation(() => now);
  vi.spyOn(document, 'hidden', 'get').mockImplementation(() => hidden);
});
afterEach(() => {
  stop?.();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});
async function start(fetchFn?: typeof fetch) {
  let opened = false;
  const client = createSessionAwareAPIClient(fetchFn ?? (async (input) => {
    const request = input as Request;
    if (opened) requests.push(request);
    opened = true;
    return Response.json({ status: 'queued' }, { status: 202 });
  }), () => 'csrf-token');
  const telemetry = startTelemetryReporting(client);
  const reporter = startSessionReporting(client);
  stop = () => { reporter.stop(); telemetry.stop(); };
  await vi.waitFor(() => expect(localStorage.length).toBeGreaterThan(0));
  return reporter;
}
function visibility(value: boolean) {
  hidden = value;
  document.dispatchEvent(new Event('visibilitychange'));
}
function close() {
  window.dispatchEvent(new PageTransitionEvent('pagehide'));
}
async function bucket(index = 0) {
  return (await requests[index].clone().json()).properties.duration_bucket;
}

it.each([
  [0, 'under_1m'], [59_999, 'under_1m'], [60_000, '1_to_5m'],
  [300_000, '5_to_30m'],
  [1_800_000, '5_to_30m'], [1_800_001, 'over_30m'],
])('reports %i visible milliseconds once as %s', async (elapsed, expected) => {
  await start();
  now = elapsed;
  close();
  close();
  expect(requests).toHaveLength(1);
  expect(await bucket()).toBe(expected);
  expect(requests[0].keepalive).toBe(true);
  expect(requests[0].credentials).toBe('same-origin');
  expect(requests[0].headers.get('X-CSRF-Token')).toBe('csrf-token');
});

it('counts twenty visible stretches without adding hidden time', async () => {
  await start();
  for (let i = 0; i < 20; i++) {
    now += 60_000;
    visibility(true);
    now += 600_000;
    visibility(false);
  }
  expect(requests).toHaveLength(0);
  close();
  expect(await bucket()).toBe('5_to_30m');
});

it('pauses while signed out and resumes earlier signed-in time', async () => {
  const reporter = await start();
  now = 40_000;
  reporter.signedIn(false);
  now = 100_000;
  reporter.signedIn(true);
  now = 110_000;
  close();
  expect(requests).toHaveLength(1);
  expect(await bucket()).toBe('under_1m');
  window.dispatchEvent(new PageTransitionEvent('pageshow'));
  now = 120_000;
  reporter.signedIn(false);
  close();
  expect(requests).toHaveLength(1);
});

it.each([false, true])('preserves signed-in time after a visible signed-out wait with delayed timers=%s', async (delayed) => {
  const reporter = await start();
  now = 40_000;
  reporter.signedIn(false);
  visibility(true);
  now += 10_000;
  vi.advanceTimersByTime(10_000);
  visibility(false);
  now += 1_800_000;
  if (delayed) vi.setSystemTime(Date.now() + 1_800_000);
  else vi.advanceTimersByTime(1_800_000);
  expect(requests).toHaveLength(0);
  reporter.signedIn(true);
  now += 30_000;
  close();
  expect(requests).toHaveLength(1);
  expect(await bucket()).toBe('1_to_5m');
});

it.each([false, true])('ends the earlier session after signing in while hidden with delayed timers=%s', async (delayed) => {
  const reporter = await start();
  now = 120_000;
  reporter.signedIn(false);
  visibility(true);
  reporter.signedIn(true);
  if (delayed) vi.setSystemTime(Date.now() + 1_800_000);
  else vi.advanceTimersByTime(1_800_000);
  visibility(false);
  expect(requests).toHaveLength(1);
  expect(await bucket()).toBe('1_to_5m');
  now += 10_000;
  close();
  expect(requests).toHaveLength(2);
  expect(await bucket(1)).toBe('under_1m');
});

it('ignores a session that was always hidden', async () => {
  hidden = true;
  await start();
  vi.advanceTimersByTime(1_800_000);
  close();
  expect(requests).toHaveLength(0);
});

it.each([false, true])('expires hidden time with delayed timers=%s', async (delayed) => {
  await start();
  now = 120_000;
  visibility(true);
  if (delayed) vi.setSystemTime(Date.now() + 1_800_000);
  else vi.advanceTimersByTime(1_800_000);
  visibility(false);
  expect(requests).toHaveLength(1);
  expect(await bucket()).toBe('1_to_5m');
  now += 10_000;
  close();
  expect(requests).toHaveLength(2);
  expect(await bucket(1)).toBe('under_1m');
});

it('removes listeners and timers on component teardown', async () => {
  await start();
  now = 120_000;
  visibility(true);
  stop();
  vi.advanceTimersByTime(1_800_000);
  visibility(false);
  close();
  expect(requests).toHaveLength(0);
  expect(vi.getTimerCount()).toBe(0);
});

it('keeps visible time while the first receipt is pending', async () => {
  let receive: (() => void) | undefined;
  const client = createSessionAwareAPIClient(async (input) => {
    const request = input as Request;
    if ((await request.clone().json()).event === 'app_opened') {
      await new Promise<void>((resolve) => { receive = resolve; });
    } else requests.push(request);
    return Response.json({ status: 'queued' }, { status: 202 });
  }, () => 'csrf-token');
  const telemetry = startTelemetryReporting(client);
  const reporter = startSessionReporting(client);
  stop = () => { reporter.stop(); telemetry.stop(); };
  await vi.waitFor(() => expect(receive).toBeDefined());
  now = 61_000;
  close();
  await vi.advanceTimersByTimeAsync(2_000);
  receive!();
  await vi.waitFor(() => expect(requests).toHaveLength(1));
  expect(await bucket()).toBe('1_to_5m');
});

it('discards visible time and listeners after a disabled receipt', async () => {
  const client = createSessionAwareAPIClient(async (input) => {
    requests.push(input as Request);
    return Response.json({ status: 'disabled' }, { status: 202 });
  }, () => 'csrf-token');
  const telemetry = startTelemetryReporting(client);
  const reporter = startSessionReporting(client);
  stop = () => { reporter.stop(); telemetry.stop(); };
  now = 120_000;
  visibility(true);
  expect(vi.getTimerCount()).toBeGreaterThan(0);
  await vi.waitFor(() => expect(localStorage.length).toBeGreaterThan(0));
  expect(vi.getTimerCount()).toBe(0);
  visibility(false);
  close();
  await Promise.resolve();
  expect(requests).toHaveLength(1);
});
