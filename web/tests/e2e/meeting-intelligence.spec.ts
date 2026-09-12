import { relationshipMeetingScope } from "../../src/lib/meetings/scopes";
import { readFile } from "node:fs/promises";
import type { Locator, Page, TestInfo } from "@playwright/test";
import {
  test,
  expect,
  meetingURL,
  loginToMeetingArchive,
} from "./fixtures/meeting-daemon";

test.beforeEach(async ({ page, daemon }) => {
  await loginToMeetingArchive(page, daemon);
});

async function assertMetrics(
  panel: Locator,
  count: number,
  known: number,
  unknown: number,
  total: string,
  average: string,
) {
  const metrics = panel.getByRole("region", {
    name: "Meeting metrics",
    exact: true,
  });
  await expect(
    metrics.getByRole("heading", { name: `${count} meetings`, exact: true }),
  ).toBeVisible();
  await expect(
    metrics.getByText(`${known} known · ${unknown} unknown duration`, {
      exact: true,
    }),
  ).toBeVisible();
  await expect(
    metrics.getByLabel("Total known meeting time", { exact: true }),
  ).toHaveText(total);
  await expect(
    metrics.getByLabel("Average known duration", { exact: true }),
  ).toHaveText(average);
}

async function downloadContext(page: Page, info: TestInfo, label: string) {
  const responsePromise = page.waitForResponse(
    (response) =>
      response.url().endsWith("/api/v1/meetings/context") &&
      response.request().method() === "POST",
  );
  const downloadPromise = page.waitForEvent("download");
  await page
    .getByRole("button", { name: "Export meeting context", exact: true })
    .click();
  const response = await responsePromise;
  expect(response.ok(), await response.text()).toBe(true);
  const result = await response.json();
  const download = await downloadPromise;
  const path = info.outputPath(`${label}.json`);
  await download.saveAs(path);
  const content = await readFile(path, "utf8");
  expect(content).toBe(result.content);
  await info.attach(label, { path, contentType: "application/json" });
  return JSON.parse(content);
}

// Regressions caught here include missing production projections, loss of raw
// provider field presence, synthetic zero durations, and source-deletion loss.
test("production imports expose archived actions, duration evidence, and exact context downloads", async ({
  page,
  daemon,
}, info) => {
  await page.goto(meetingURL(daemon));
  const panel = page.getByRole("region", {
    name: "Meeting activity",
    exact: true,
  });
  await assertMetrics(panel, 4, 3, 1, "1h 40m", "33m 20s");
  const bases = panel.getByRole("table", { name: "Duration evidence" });
  await expect(
    bases.getByRole("row", { name: "Provider duration 1 30m" }),
  ).toBeVisible();
  await expect(
    bases.getByRole("row", { name: "Scheduled 1 1h" }),
  ).toBeVisible();
  await expect(
    bases.getByRole("row", { name: "Transcript span 1 10m" }),
  ).toBeVisible();
  await expect(
    panel
      .getByRole("table", { name: "Monthly meeting activity" })
      .getByRole("row", { name: "2026-01 2 2 0 1h 30m 45m" }),
  ).toBeVisible();
  await expect(
    panel.getByText("Send the Circleback recap", { exact: true }),
  ).toBeVisible();
  await expect(
    panel.getByText("Publish the Notion recap", { exact: true }),
  ).toBeVisible();
  await expect(panel.getByText('completed (source: true)', { exact: true })).toBeVisible();
  await expect(
    panel.getByText(
      "Coverage: 3 available · 0 partial · 1 unsupported · 0 unavailable",
      { exact: true },
    ),
  ).toBeVisible();
  await page.screenshot({
    path: info.outputPath("meeting-overview.png"),
    fullPage: true,
  });

  // The production selection UI carries actual emitted row keys and authority.
  const row = page
    .getByRole("row")
    .filter({ hasText: "Generic unknown duration" });
  await expect(row).toBeVisible();
  await page.getByRole("grid", { name: "Everything results" }).focus();
  await page.keyboard.press("Home");
  await page.keyboard.press("Space");
  const explicit = await downloadContext(page, info, "explicit-context");
  expect(explicit.meetings).toHaveLength(1);
  expect(explicit.meetings[0].meeting.message_id).toBe(
    daemon.generic.message_id,
  );
  expect(explicit.meetings[0].content.summary.state).toBe("unavailable");
  expect(explicit.meetings[0].content.action_coverage).toBe("available");
  expect(explicit.meetings[0].content.actions).toEqual([]);
  expect(JSON.stringify(explicit)).not.toContain("Generic transcript evidence");
  await page
    .getByRole("button", { name: "Select all 4 matching items", exact: true })
    .click();
  const all = await downloadContext(page, info, "all-matching-context");
  expect(
    all.meetings
      .map(
        (entry: { meeting: { message_id: number } }) =>
          entry.meeting.message_id,
      )
      .sort(),
  ).toEqual(
    [
      daemon.generic.message_id,
      ...Object.values(daemon.meetings).map((meeting) => meeting.message_id),
    ].sort(),
  );
  expect(
    new Set(
      all.meetings.map(
        (entry: { meeting: { source_type: string } }) =>
          entry.meeting.source_type,
      ),
    ),
  ).toEqual(
    new Set(["granola", "notion_meetings", "circleback", "meeting_import"]),
  );
  await page
    .getByRole("checkbox", { name: "Include transcript", exact: true })
    .check();
  const transcript = await downloadContext(page, info, "transcript-context");
  expect(JSON.stringify(transcript)).toContain(
    "Generic transcript evidence without a summary or timing.",
  );
  expect(JSON.stringify(transcript)).toContain(
    "Circleback transcript evidence.",
  );
  await page
    .getByRole("button", { name: "Clear selection", exact: true })
    .click();

  await panel.getByRole("combobox", { name: "Source status" }).click();
  await page.getByRole("option", { name: "Pending", exact: true }).click();
  await panel
    .getByRole("textbox", { name: "Assignee email", exact: true })
    .fill("alex@example.com");
  await panel.getByRole("button", { name: "Apply action filters" }).click();
  await expect(
    panel.getByText("1 matching action items", { exact: true }),
  ).toBeVisible();
  await expect(
    panel.getByText("Publish the Notion recap", { exact: true }),
  ).toHaveCount(0);
  const source = panel.getByRole("link", {
    name: "Open archived meeting",
    exact: true,
  });
  await source.click();
  const reader = page.getByRole("dialog", {
    name: "Archived meeting",
    exact: true,
  });
  await expect(reader).toBeVisible();
  await expect(
    reader.getByText("Circleback provider half hour", { exact: true }).first(),
  ).toBeVisible();
  await expect
    .poll(() =>
      page.evaluate(
        () =>
          JSON.parse(new URL(location.href).searchParams.get("explore")!)
            .selectedRow,
      ),
    )
    .toBe(`archive-meeting:${daemon.meetings.circleback.message_id}`);
  await page.screenshot({
    path: info.outputPath("source-deleted-reader.png"),
    fullPage: true,
  });
  await page.goBack();
  await expect(reader).toHaveCount(0);
  await expect(source).toBeFocused();
  await expect(
    panel.getByRole("textbox", { name: "Assignee email" }),
  ).toHaveValue("alex@example.com");
  await page.goForward();
  await expect(reader).toBeVisible();
  // An action link inside the single-meeting reader opens its archived source
  // without losing the originating Everything state.
  await reader
    .getByRole("link", { name: "Open archived meeting", exact: true })
    .click();
  await expect(reader).toBeVisible();
  await page.goBack();
  await expect(reader).toBeVisible();
  await page.reload();
  await expect(
    reader.getByText("Circleback provider half hour", { exact: true }).first(),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Close archived meeting", exact: true })
    .click();
  await expect(reader).toHaveCount(0);

  const deleted = await daemon.post("meetings/metrics", {
    scope: { deletion: "deleted" },
  });
  expect((await deleted.json()).totals).toMatchObject({
    meeting_count: 1,
    known_duration_count: 1,
    total_known_seconds: 1800,
  });
  const unknown = await daemon.post("meetings/metrics", {
    scope: { message_ids: [daemon.generic.message_id] },
  });
  expect((await unknown.json()).totals).toMatchObject({
    meeting_count: 1,
    known_duration_count: 0,
    unknown_duration_count: 1,
    average_known_seconds: null,
  });
});

test("participant, domain, Directory and Relationships keep scoped meeting evidence and source navigation", async ({
  page,
  daemon,
}, info) => {
  const archive = await daemon.post("meetings/metrics", { scope: {} });
  expect((await archive.json()).totals.meeting_count).toBe(4);
  const scopes: Array<{ name: string; state: Record<string, unknown> }> = [
    {
      name: "participant",
      state: {
        groupingChain: ["participant"],
        selectedRow: `group:participant:${daemon.participant_id}`,
      },
    },
    {
      name: "domain",
      state: {
        groupingChain: ["domain"],
        selectedRow: "group:domain:example.com",
      },
    },
    {
      name: "directory",
      state: { workspace: "directory", directoryPersonID: daemon.personID },
    },
    {
      name: "relationships",
      state: {
        workspace: "relationships",
        relationshipFacet: "people",
        relationshipTarget: `cluster:${daemon.participant_id}`,
        relationshipShowAll: true,
      },
    },
  ];
  for (const scope of scopes) {
    await page.goto(meetingURL(daemon, scope.state));
    const panel = page
      .getByRole("region", { name: "Meeting activity", exact: true })
      .last();
    await assertMetrics(panel, 3, 3, 0, "1h 40m", "33m 20s");
    const source = panel
      .getByRole("link", { name: "Open archived meeting", exact: true })
      .filter({ visible: true })
      .first();
    await source.click();
    const reader = page.getByRole("dialog", {
      name: "Archived meeting",
      exact: true,
    });
    await expect(reader).toBeVisible();
    await page.goBack();
    await expect(reader).toHaveCount(0);
    await expect(source).toBeFocused();
    await page.screenshot({
      path: info.outputPath(`${scope.name}-meetings.png`),
      fullPage: true,
    });
  }
  await page.setViewportSize({ width: 700, height: 900 });
  await page.goto(
    meetingURL(daemon, {
      workspace: "directory",
      directoryPersonID: daemon.personID,
    }),
  );
  const narrow = page.getByRole("region", {
    name: "Meeting activity",
    exact: true,
  });
  await assertMetrics(narrow, 3, 3, 0, "1h 40m", "33m 20s");
  await narrow
    .getByRole("link", { name: "Open archived meeting" })
    .first()
    .click();
  await expect(
    page.getByRole("dialog", { name: "Archived meeting", exact: true }),
  ).toBeVisible();
  await page.goBack();
  await expect(
    page.getByRole("dialog", { name: "Archived meeting", exact: true }),
  ).toHaveCount(0);
});

test("rapid search scope changes leave only the final meeting evidence", async ({
  page,
  daemon,
}, info) => {
  await page.goto(meetingURL(daemon));
  const panel = page.getByRole("region", {
    name: "Meeting activity",
    exact: true,
  });
  await assertMetrics(panel, 4, 3, 1, "1h 40m", "33m 20s");
  const search = page.getByRole("searchbox", {
    name: "Search everything",
    exact: true,
  });
  const firstRequest = page.waitForRequest(
    (request) =>
      request.url().endsWith("/api/v1/meetings/metrics") &&
      request.postDataJSON()?.explore?.predicate?.query === "Circleback",
  );
  await search.fill("Circleback");
  await search.press("Enter");
  await firstRequest;
  await search.fill("Generic");
  await search.press("Enter");
  await assertMetrics(panel, 1, 0, 1, "0s", "Unavailable");
  await expect(
    panel.getByText("0 matching action items", { exact: true }),
  ).toBeVisible();
  await expect(
    panel.getByText("Send the Circleback recap", { exact: true }),
  ).toHaveCount(0);
  await expect(
    panel.getByText("No recorded action items", { exact: true }),
  ).toBeVisible();
  await page.screenshot({
    path: info.outputPath("unknown-duration-final-scope.png"),
    fullPage: true,
  });
});


test("contradictory Relationships dates produce a valid empty API population", async ({ daemon }) => {
  const mapped = relationshipMeetingScope({ participant_id: daemon.participant_id }, { filters: [
    { dimension: 'after', values: ['2026-02-01T00:00:00Z'] },
    { dimension: 'before', values: ['2026-01-01T00:00:00Z'] },
  ] });
  if (mapped.kind !== 'direct') throw new Error('Expected direct match-none scope');
  for (const endpoint of ['meetings/metrics', 'meetings/actions']) {
    const response = await daemon.post(endpoint, { scope: mapped.scope });
    expect(response.status, await response.clone().text()).toBe(200);
    const result = await response.json();
    if (endpoint.endsWith('metrics')) expect(result.totals.meeting_count).toBe(0);
    else { expect(result.total_count).toBe(0); expect(result.coverage.meeting_count).toBe(0); }
  }
});

async function importLongActions(daemon: import('./fixtures/meeting-daemon').MeetingDaemon) {
  const response = await daemon.post('import/meeting', {
    source: { identifier: 'fixture-long-actions', account_email: 'owner@example.org' },
    meeting: {
      external_id: 'long-actions', title: 'Long action evidence', started_at: '2026-03-01T10:00:00Z',
      attendees: [{ name: 'Alex Example', email: 'alex@example.com' }],
      transcript: 'Reachable transcript after many archived actions.',
      action_items: Array.from({ length: 201 }, (_, index) => ({
        title: `Archived action ${index + 1}`,
        description: 'Recorded source details remain readable. '.repeat(12),
        status: 'open',
      })),
    },
  });
  expect(response.status, await response.clone().text()).toBe(201);
  return response.json() as Promise<{ message_id: number }>;
}

for (const viewport of [{ width: 1280, height: 720 }, { width: 700, height: 560 }]) {
  test(`reader scrolls long evidence and reaches action 201 at ${viewport.width}`, async ({ page, daemon }, info) => {
    const imported = await importLongActions(daemon);
    await page.setViewportSize(viewport);
    await page.goto(meetingURL(daemon, { selectedRow: `archive-meeting:${imported.message_id}` }));
    const reader = page.getByRole('dialog', { name: 'Archived meeting', exact: true });
    const actions = reader.getByRole('region', { name: 'Archived action items', exact: true });
    await expect(actions.getByText('Archived action 1', { exact: true })).toBeVisible();
    const later = actions.getByText('Archived action 200', { exact: true });
    await later.scrollIntoViewIfNeeded();
    await expect(later).toBeInViewport();
    const transcript = reader.getByText('Reachable transcript after many archived actions.', { exact: false }).last();
    await transcript.evaluate((element) => element.scrollIntoView({ block: 'end' }));
    // Check the actual transcript text range, not just the giant pre element
    // that also contains the importer's textual copy of every action.
    await expect.poll(() => transcript.evaluate((element) => {
      const marker = 'Reachable transcript after many archived actions.';
      const walker = document.createTreeWalker(element, NodeFilter.SHOW_TEXT);
      let text: Node | null;
      while ((text = walker.nextNode())) {
        const offset = text.textContent?.indexOf(marker) ?? -1;
        if (offset < 0) continue;
        const range = document.createRange();
        range.setStart(text, offset);
        range.setEnd(text, offset + marker.length);
        const rect = range.getBoundingClientRect();
        return rect.height > 0 && rect.top >= 0 && rect.bottom <= innerHeight &&
          element.contains(document.elementFromPoint(rect.left + 2, rect.top + 2));
      }
      return false;
    })).toBe(true);
    await page.screenshot({ path: info.outputPath(`reader-scroll-${viewport.width}.png`), fullPage: true });
    const continuation = page.waitForResponse(response => response.url().endsWith('/api/v1/meetings/actions') && !!response.request().postDataJSON()?.cursor);
    await actions.getByRole('button', { name: 'Load more action items', exact: true }).click();
    expect((await continuation).ok()).toBe(true);
    const final = actions.getByText('Archived action 201', { exact: true });
    await final.scrollIntoViewIfNeeded();
    await expect(final).toBeInViewport();
    await expect(actions.getByText('Showing 201 of 201 action items', { exact: true })).toBeVisible();
    await page.screenshot({ path: info.outputPath(`reader-final-action-${viewport.width}.png`), fullPage: true });
  });
}


test('closing the reader removes its scope and cancels a pending continuation', async ({ page, daemon }) => {
  await importLongActions(daemon);
  await page.goto(meetingURL(daemon, { workspace: 'directory', directoryPersonID: daemon.personID }));
  const panel = page.getByRole('region', { name: 'Meeting activity', exact: true });
  const firstAction = panel.getByText('Archived action 1', { exact: true });
  await expect(firstAction).toBeVisible();
  await firstAction.locator('xpath=ancestor::li').getByRole('link', { name: 'Open archived meeting', exact: true }).click();
  const reader = page.getByRole('dialog', { name: 'Archived meeting', exact: true });
  await expect(reader.getByText('Archived action 1', { exact: true })).toBeVisible();
  // Browser transport latency keeps a genuine daemon request pending; no
  // meeting response or endpoint is intercepted or replaced.
  const network = await page.context().newCDPSession(page);
  await network.send('Network.enable');
  await network.send('Network.emulateNetworkConditions', { offline: false, latency: 1500, downloadThroughput: -1, uploadThroughput: -1 });
  try {
    const pending = page.waitForRequest(request => request.url().endsWith('/api/v1/meetings/actions') && !!request.postDataJSON()?.cursor);
    await reader.getByRole('button', { name: 'Load more action items', exact: true }).click();
    const request = await pending;
    const cancelled = page.waitForEvent('requestfailed', { predicate: candidate => candidate === request });
    await reader.getByRole('button', { name: 'Close archived meeting', exact: true }).click();
    await cancelled;
    await expect(reader).toHaveCount(0);
    await expect(panel.getByText('Archived action 1', { exact: true })).toBeVisible();
    await expect.poll(() => JSON.parse(new URL(page.url()).searchParams.get('explore')!).workspace).toBe('directory');
    await network.send('Network.emulateNetworkConditions', { offline: false, latency: 0, downloadThroughput: -1, uploadThroughput: -1 });
    await page.getByRole('button', { name: 'Everything', exact: true }).click();
    const search = page.getByRole('searchbox', { name: 'Search everything', exact: true });
    await search.fill('Generic');
    await search.press('Enter');
    const finalPanel = page.getByRole('region', { name: 'Meeting activity', exact: true });
    await expect(finalPanel.getByText('No recorded action items', { exact: true })).toBeVisible();
    await expect(finalPanel.getByText('Archived action 201', { exact: true })).toHaveCount(0);
  } finally {
    if (!page.isClosed()) {
      await network.send('Network.emulateNetworkConditions', { offline: false, latency: 0, downloadThroughput: -1, uploadThroughput: -1 });
      await network.detach();
    }
  }
});
