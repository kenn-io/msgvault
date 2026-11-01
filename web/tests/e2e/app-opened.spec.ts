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
  expect(await response.json()).toEqual({ status: "disabled" });
  expect(response.request().postDataJSON()).toEqual({
    event: "app_opened",
    properties: { surface: "web" },
  });
});

test("a disabled daemon stays quiet across a two-minute visit and reload", async ({ page, daemon }) => {
  // Browser routing can abort keepalive requests during unload. Observe the real HTTP exchange instead.
  const deliveries: { body: unknown; status: number | undefined; receipt: unknown }[] = [];
  const proxy = createServer((incoming, outgoing) => {
    const chunks: Buffer[] = [];
    const isTelemetry = incoming.method === "POST" && incoming.url === "/api/v1/telemetry/events";
    if (isTelemetry) incoming.on("data", (chunk: Buffer) => chunks.push(chunk));
    const upstream = request(new URL(incoming.url!, daemon.origin), {
      method: incoming.method,
      headers: incoming.headers,
    }, (response) => {
      if (isTelemetry) {
        const receipt: Buffer[] = [];
        response.on("data", (chunk: Buffer) => receipt.push(chunk));
        response.on("end", () => {
          deliveries.push({
            body: JSON.parse(Buffer.concat(chunks).toString()),
            status: response.statusCode,
            receipt: JSON.parse(Buffer.concat(receipt).toString()),
          });
        });
      }
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
    await expect.poll(() => deliveries).toEqual([{
      status: 202,
      body: { event: "app_opened", properties: { surface: "web" } },
      receipt: { status: "disabled" },
    }]);
    await page.clock.runFor(120_000);
    await page.reload();
    await expect(page.getByRole("button", { name: "Settings", exact: true })).toBeVisible();
    expect(deliveries).toEqual([{
      status: 202,
      body: { event: "app_opened", properties: { surface: "web" } },
      receipt: { status: "disabled" },
    }]);
  } finally {
    proxy.closeAllConnections();
    await new Promise<void>((resolve, reject) => proxy.close((error) => error ? reject(error) : resolve()));
  }
});
