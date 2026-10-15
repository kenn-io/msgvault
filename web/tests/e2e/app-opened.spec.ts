import { test, expect, loginToMeetingArchive } from "./fixtures/meeting-daemon";
import { createServer, request } from "node:http";
import { once } from "node:events";
import type { AddressInfo } from "node:net";

test("logging in reports app_opened through the daemon", async ({
  page,
  daemon,
}) => {
  const telemetry = page.waitForResponse(
    (response) =>
      response.request().method() === "POST" &&
      new URL(response.url()).pathname === "/api/v1/telemetry/events" &&
      response.request().postDataJSON()?.event === "app_opened",
  );
  await loginToMeetingArchive(page, daemon);
  const response = await telemetry;
  expect(response.status()).toBe(202);
  expect(response.request().postDataJSON()).toEqual({
    event: "app_opened",
    properties: { surface: "web" },
  });
});

test("reloading a two-minute visit reports its duration through the daemon", async ({ page, daemon }) => {
  // Browser routing can abort keepalive requests during unload. Observe the real HTTP exchange instead.
  const deliveries: { body: unknown; status: number | undefined }[] = [];
  const proxy = createServer((incoming, outgoing) => {
    const chunks: Buffer[] = [];
    const isTelemetry = incoming.method === "POST" && incoming.url === "/api/v1/telemetry/events";
    if (isTelemetry) incoming.on("data", (chunk: Buffer) => chunks.push(chunk));
    const upstream = request(new URL(incoming.url!, daemon.origin), {
      method: incoming.method,
      headers: incoming.headers,
    }, (response) => {
      if (isTelemetry) deliveries.push({ body: JSON.parse(Buffer.concat(chunks).toString()), status: response.statusCode });
      outgoing.writeHead(response.statusCode!, response.headers);
      response.pipe(outgoing);
    });
    upstream.on("error", (error) => outgoing.destroy(error));
    incoming.pipe(upstream);
  });
  proxy.listen(0, "127.0.0.1");
  await once(proxy, "listening");
  try {
    const origin = `http://127.0.0.1:${(proxy.address() as AddressInfo).port}`;
    await page.clock.install();
    await loginToMeetingArchive(page, { ...daemon, origin });
    await page.clock.runFor(120_000);
    await page.reload();
    await expect.poll(() => deliveries).toContainEqual({
      status: 202,
      body: { event: "session_ended", properties: { surface: "web", duration_bucket: "1_to_5m" } },
    });
  } finally {
    proxy.closeAllConnections();
    await new Promise<void>((resolve, reject) => proxy.close((error) => error ? reject(error) : resolve()));
  }
});
