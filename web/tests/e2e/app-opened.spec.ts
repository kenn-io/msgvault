import { test, expect, loginToMeetingArchive } from "./fixtures/meeting-daemon";

test("logging in reports app_opened through the daemon", async ({ page, daemon }) => {
  const telemetry = page.waitForResponse(
    (response) =>
      response.request().method() === "POST" &&
      new URL(response.url()).pathname === "/api/v1/telemetry/events",
  );
  await loginToMeetingArchive(page, daemon);
  const response = await telemetry;
  expect(response.status()).toBe(202);
  expect(response.request().postDataJSON()).toEqual({ event: "app_opened", properties: { surface: "web" } });
  expect(response.request().headers()["x-csrf-token"]).toBeTruthy();
});
