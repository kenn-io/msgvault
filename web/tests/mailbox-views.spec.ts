import { expect, test } from "@playwright/test";
import { selectWorkspace, stubOperationStatus } from "./kit-ui";

// The native Go integration covers sync -> cache -> authenticated daemon -> MCP.
// This fixture isolates the browser's filter/save/reopen and responsive layout.
for (const width of [1280, 390]) {
  test(`Inbox Unread saved view remains usable at ${width}px`, async ({
    page,
  }) => {
    await page.setViewportSize({ width, height: 850 });
    const saved: Array<Record<string, unknown>> = [];
    const predicates: Array<Record<string, unknown>> = [];
    await stubOperationStatus(page);
    await page.route("**/api/session", (route) =>
      route.fulfill({
        json: {
          auth_mode: "session",
          csrf_token: "synthetic-csrf",
          https: true,
          plain_http_warning: false,
        },
      }),
    );
    await page.route("**/api/v1/sources/status", (route) =>
      route.fulfill({ json: { sources: [] } }),
    );
    await page.route("**/api/v1/cli/accounts", (route) =>
      route.fulfill({ json: { accounts: [] } }),
    );
    await page.route("**/api/v1/explore/groups", (route) =>
      route.fulfill({ json: { rows: [], total_count: 0 } }),
    );
    await page.route("**/api/v1/explore", (route) => {
      predicates.push(route.request().postDataJSON());
      return route.fulfill({
        json: {
          rows: [
            {
              key: "message:1",
              kind: "email",
              anchor_message_id: 1,
              source_id: 1,
              source_type: "gmail",
              source_identifier: "owner@example.test",
              message_type: "email",
              conversation_type: "email",
              title: "Waiting for review",
              preview: "Synthetic mailbox fixture",
              occurred_at: "2026-11-01T12:00:00Z",
              inbox_state: "inbox",
              read_state: "unread",
              match: {},
              message_count: 1,
              has_attachments: false,
              attachment_count: 0,
              attachment_size: 0,
              deleted_from_source: false,
              matched_sender_identities: [],
              matched_recipient_identities: [],
            },
          ],
          total_count: 1,
          cache_revision: "mailbox-fixture",
          search_provenance: {},
        },
      });
    });
    await page.route("**/api/v1/saved-views", async (route) => {
      if (route.request().method() === "POST") {
        const view = {
          ...route.request().postDataJSON(),
          id: 1,
          revision: 1,
          created_at: "2026-11-01T12:00:00Z",
          updated_at: "2026-11-01T12:00:00Z",
        };
        saved.push(view);
        return route.fulfill({ status: 201, json: view });
      }
      return route.fulfill({ json: { saved_views: saved } });
    });
    await page.goto("/?workspace=everything");
    await page.getByRole("button", { name: "Filter", exact: true }).click();
    await page.getByRole("checkbox", { name: "In Inbox", exact: true }).click();
    await page
      .getByRole("checkbox", { name: "Unread at provider", exact: true })
      .click();
    await page.keyboard.press("Escape");
    await expect(
      page.getByText("Provider Inbox: In Inbox", { exact: true }),
    ).toBeVisible();
    await expect(
      page.getByText("Provider read status: Unread at provider", {
        exact: true,
      }),
    ).toBeVisible();
    await expect
      .poll(() => predicates.at(-1)?.filters)
      .toEqual([
        { dimension: "inbox_state", values: ["inbox"] },
        { dimension: "read_state", values: ["unread"] },
      ]);
    await page.getByRole("button", { name: "Save view…", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "Save view", exact: true });
    await dialog
      .getByRole("textbox", { name: "Name", exact: true })
      .fill("Inbox Unread");
    await dialog.getByRole("button", { name: "Save", exact: true }).click();
    await expect(dialog).toHaveCount(0);
    expect(saved[0]?.canonical_state).toMatchObject({
      filters: [
        { field: "inbox_state", operator: "in", values: ["inbox"] },
        { field: "read_state", operator: "in", values: ["unread"] },
      ],
    });
    await page.goto("/?workspace=everything");
    await selectWorkspace(page, "Inbox Unread");
    const chips = page.getByRole("region", {
      name: "Active analytical context",
    });
    await expect(
      chips.getByRole("button", { name: "Remove Provider Inbox filter" }),
    ).toBeVisible();
    await expect(
      chips.getByRole("button", { name: "Remove Provider read status filter" }),
    ).toBeVisible();
    for (const chip of await chips.locator(".chip--filter").all()) {
      const bounds = await chip.boundingBox();
      expect(bounds).not.toBeNull();
      expect(bounds!.x).toBeGreaterThanOrEqual(0);
      expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(width);
    }
    await expect(
      page.getByText("In Inbox · Unread at provider", { exact: true }),
    ).toHaveCount(1);
  });
}
