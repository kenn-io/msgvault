import { test, expect, loginToMeetingArchive } from "./fixtures/meeting-daemon";
import type { APIResponse, Request } from "@playwright/test";

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
  await page.clock.install();
  await loginToMeetingArchive(page, daemon);
  await page.clock.runFor(120_000);
  // Unload responses outlive the page; forward to the real daemon from the browser context.
  let deliver!: (result: { request: Request; response: APIResponse }) => void;
  const telemetry = new Promise<{ request: Request; response: APIResponse }>((resolve) => {
    deliver = resolve;
  });
  await page.context().route("**/api/v1/telemetry/events", async (route) => {
    if (route.request().postDataJSON()?.event !== "session_ended") return route.continue();
    const response = await route.fetch();
    await route.fulfill({ response });
    deliver({ request: route.request(), response });
  });
  await page.reload();
  const { request, response } = await telemetry;
  expect(response.status()).toBe(202);
  expect(request.postDataJSON()).toEqual({
    event: "session_ended", properties: { surface: "web", duration_bucket: "1_to_5m" },
  });
});
