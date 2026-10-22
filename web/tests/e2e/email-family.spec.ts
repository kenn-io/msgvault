import { expect, test } from "@playwright/test";

test("related copies preview, confirm, history, undo and provenance download", async ({
  page,
}) => {
  const message = {
    id: 42,
    source_id: 1,
    source_message_id: "seed",
    conversation_id: 7,
    subject: "Archived request",
    message_type: "email",
    from: "sender@example.test",
    to: ["owner@example.test"],
    sent_at: "2026-01-01T12:00:00Z",
    snippet: "Synthetic request",
    labels: [],
    has_attachments: false,
    size_bytes: 20,
    body: "Synthetic original",
    attachments: [],
  };
  const members = [42, 43].map((id) => ({
    message_id: id,
    source_id: id,
    source_identifier: `account-${id}@example.test`,
    account_address: "",
    subject: `Copy ${id}`,
    linkage: { outcome: "complete" },
    labels: [],
    evidence_kinds: ["self"],
    sent_at: "2026-01-01T12:00:00Z",
  }));
  let revision = "before";
  let state = "active";
  let writes = 0;
  const family = () => ({
    anchor_message_id: 42,
    members,
    handling: { state: "unknown", reasons: ["provider_evidence_unavailable"] },
    truncated: false,
    revision,
    decisions: [],
  });
  const decision = () => ({
    id: 9,
    revision: state === "active" ? 1 : 2,
    kind: "handled",
    state,
    message_ids: [43],
  });
  await page.route("**/api/session", (route) =>
    route.fulfill({
      json: { auth_mode: "loopback", https: false, plain_http_warning: false },
    }),
  );
  await page.route("**/api/v1/**", async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    if (path === "/api/v1/settings")
      return route.fulfill({ json: { settings: [], pending_restart: false } });
    if (path === "/api/v1/telemetry/events")
      return route.fulfill({ status: 202, json: { status: "disabled" } });
    if (path === "/api/v1/integrations/kata/status")
      return route.fulfill({ json: { state: "disabled", project: "" } });
    if (path === "/api/v1/messages/42") return route.fulfill({ json: message });
    if (path === "/api/v1/conversations/7")
      return route.fulfill({
        json: {
          id: 7,
          anchor_id: 42,
          messages: [message],
          has_before: false,
          has_after: false,
          total: 1,
        },
      });
    if (path === "/api/v1/messages/42/family")
      return route.fulfill({ json: family() });
    if (path.endsWith("/decisions/preview")) {
      expect(request.postDataJSON().message_ids).toEqual([43]);
      return route.fulfill({
        json: { before_revision: revision, family: family() },
      });
    }
    if (
      path === "/api/v1/messages/family/decisions" &&
      request.method() === "POST"
    ) {
      expect(request.postDataJSON().expected_family_revision).toBe("before");
      writes++;
      revision = "after";
      return route.fulfill({
        json: { decision: decision(), family: family() },
      });
    }
    if (path === "/api/v1/messages/family/decisions")
      return route.fulfill({ json: [decision()] });
    if (path === "/api/v1/messages/family/decisions/9")
      return route.fulfill({ json: { decision: decision(), family: family() } });
    if (path.endsWith("/decisions/9/undo")) {
      expect(request.postDataJSON().expected_family_revision).toBe("after");
      state = "undone";
      revision = "undone";
      return route.fulfill({
        json: { decision: decision(), family: family() },
      });
    }
    if (path.endsWith("/family/export")) {
      expect(request.postDataJSON().expected_family_revision).toBe("undone");
      return route.fulfill({
        contentType: "application/zip",
        body: Buffer.from("PK synthetic archive"),
        headers: {
          "Content-Disposition": 'attachment; filename="email-family-42.zip"',
        },
      });
    }
    return route.fulfill({
      status: 404,
      json: { message: "Unexpected archive request" },
    });
  });
  await page.setViewportSize({ width: 480, height: 960 });
  await page.goto("/messages/42");
  await page.getByRole("button", { name: "Related copies (2)" }).click();
  await page.getByRole("checkbox", { name: "Select copy 43" }).check();
  await page.getByRole("button", { name: "Preview handled" }).click();
  await expect(
    page.getByRole("region", { name: "Decision preview" }),
  ).toContainText("43");
  expect(writes).toBe(0);
  await page.getByRole("button", { name: "Confirm decision" }).click();
  await expect(
    page.getByRole("button", { name: "Undo last decision" }),
  ).toBeVisible();
  expect(writes).toBe(1);
  await page.getByRole("button", { name: "Decision history" }).click();
  await expect(
    page.getByRole("heading", { name: "Recent decisions" }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Undo decision 9" }).click();
  await expect(
    page.getByRole("button", { name: "Undo last decision" }),
  ).toHaveCount(0);
  const downloaded = page.waitForEvent("download");
  await page.getByRole("button", { name: "Download originals ZIP" }).click();
  expect((await downloaded).suggestedFilename()).toBe("email-family-42.zip");
  await expect(page.getByRole("link", { name: "Copy 43" })).toHaveAttribute(
    "href",
    "/messages/43",
  );
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth - innerWidth,
    ),
  ).toBeLessThanOrEqual(1);
  await page.screenshot({
    path: "test-results/artifacts/email-family.png",
    fullPage: true,
  });
});
