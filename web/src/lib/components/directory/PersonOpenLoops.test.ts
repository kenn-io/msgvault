import { render, screen } from "@testing-library/svelte";
import { describe, expect, it, vi } from "vitest";
import { tick } from "svelte";
import { createAPIClient } from "../../api/client";
import PersonOpenLoops from "./PersonOpenLoops.svelte";

function loop(id = 42, kind = "reply_owed", summary = "Review the draft?") {
  return {
    kind,
    anchor_message_id: id,
    conversation_id: id + 65,
    source_id: 1,
    channel: "email",
    counterparty: {
      participant_id: 3,
      display_name: "Alex",
      address: "a@x.test",
    },
    summary,
    owed_since_at: new Date(Date.now() - 2 * 86400_000).toISOString(),
    last_activity_at: new Date(Date.now() - 86400_000).toISOString(),
  };
}

function promise(id: number, text = `Promise ${id}`, messageID = id) {
  return { id: `c${id}`, person_id: 7, summary: text, state: "open", direction: "owed_by", freshness: "fresh", promise: { message_id: messageID, start: 0, end: [...text].length, text }, promised_at: "2026-10-12T12:00:00Z" };
}

function clientFor(
  items: ReturnType<typeof loop>[],
  truncated = false,
  skipped = 0,
  coverageIncomplete = false,
) {
  const fetcher = vi.fn<typeof fetch>(async () =>
    Response.json({
      items,
      truncated,
      skipped_unparseable: skipped,
      coverage_incomplete: coverageIncomplete,
    }),
  );
  return { client: createAPIClient(fetcher), fetcher };
}

describe("PersonOpenLoops", () => {
  it("lists a person's loops with reader links", async () => {
    const { client, fetcher } = clientFor([
      loop(42),
      loop(43, "awaiting_reply", ""),
    ]);
    render(PersonOpenLoops, { client, personID: 7 });
    expect(screen.getByRole("heading", { name: "Open loops" })).toBeDefined();
    expect(screen.getByRole("status").textContent).toContain("Loading");
    const link = await screen.findByRole("link", { name: "Review the draft?" });
    expect(link.getAttribute("href")).toBe("/messages/42");
    expect(
      screen.getByRole("link", { name: "(no subject)" }).getAttribute("href"),
    ).toBe("/messages/43");
    expect(screen.getByText("Waiting on you")).toBeDefined();
    expect(screen.getByText("Waiting on them")).toBeDefined();
    expect(screen.getAllByText("2d ago")).toHaveLength(2);
    expect(screen.getByText("2 open")).toBeDefined();
    expect(screen.queryByText(/could not be read/)).toBeNull();
    const request = fetcher.mock.calls[0]?.[0];
    const url = new URL(
      request instanceof Request ? request.url : String(request),
      "http://localhost",
    );
    expect(url.pathname).toBe("/api/v1/loops");
    expect(url.searchParams.get("person_id")).toBe("7");
    expect(url.searchParams.get("limit")).toBe("5");
  });

  it("shows fresh and stale cited promises and includes them in the count", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date(2026, 0, 15, 12));
    try {
      const fetcher = vi.fn<typeof fetch>(async () => Response.json({
        items: [loop(42), loop(43), loop(44), loop(45), loop(46)],
        truncated: true,
        commitments: [
          { ...promise(12, "I will send the draft.", 52), summary: "Send the draft", promised_at: "2026-01-14T12:00:00Z", due_on: "2026-01-20" },
          { ...promise(13, "I will check the venue.", 53), summary: "Check the venue", direction: "owed_to", freshness: "stale", promised_at: "2026-01-14T12:00:00Z", due_on: "2026-01-15", overdue: true },
        ],
      }));
      render(PersonOpenLoops, { client: createAPIClient(fetcher), personID: 7 });
      const cite = await screen.findByRole("link", { name: "“I will send the draft.”" });
      expect(cite.getAttribute("href")).toBe("/messages/52");
      expect(screen.getByRole("heading", { name: "Promises you made" })).toBeDefined();
      expect(screen.getByRole("heading", { name: "Stale promises to you" })).toBeDefined();
      const stale = screen.getByRole("link", { name: "“I will check the venue.”" });
      expect(stale.getAttribute("href")).toBe("/messages/53");
      expect(await screen.findByText("7+ open")).toBeDefined();
      expect(cite.closest("li")?.textContent).toContain("due Jan 20");
      expect(cite.closest("li")?.textContent).toMatch(/made \d+[dhm] ago/);
      expect(stale.closest("li")?.textContent).toContain("overdue Jan 15");
    } finally { vi.useRealTimers(); }
  });

  it("shows a plus when more loops are open than the card lists", async () => {
    const { client } = clientFor(
      [loop(1), loop(2), loop(3), loop(4), loop(5)],
      true,
    );
    render(PersonOpenLoops, { client, personID: 7 });
    expect(await screen.findByText("5+ open")).toBeDefined();
  });

  it("says how many messages could not be read, with or without loops", async () => {
    const populated = clientFor([loop(1)], false, 2);
    const view = render(PersonOpenLoops, { client: populated.client, personID: 7 });
    expect(await screen.findByRole("link", { name: "Review the draft?" })).toBeDefined();
    expect(
      screen.getByText(
        "2 messages could not be read, so a reply in one may have settled a loop.",
      ),
    ).toBeDefined();
    view.unmount();

    const empty = clientFor([], false, 1);
    render(PersonOpenLoops, { client: empty.client, personID: 7 });
    expect(
      await screen.findByText("Nothing open with this person."),
    ).toBeDefined();
    expect(
      screen.getByText(
        "1 message could not be read, so a reply in it may have settled a loop.",
      ),
    ).toBeDefined();
    expect(screen.queryByText(/open$/)).toBeNull();
  });

  it("warns when the scan could not cover all stored originals", async () => {
    const { client } = clientFor([loop()], false, 0, true);
    const view = render(PersonOpenLoops, { client, personID: 7 });
    expect(await screen.findByRole("button", { name: "Check more" })).toBeDefined();
    view.unmount();

    const empty = clientFor([], false, 0, true);
    render(PersonOpenLoops, { client: empty.client, personID: 7 });
    expect(await screen.findByText("No open loops found. Some messages haven't been checked yet."))
      .toBeDefined();
  });

  it("checks more archive history when coverage is incomplete", async () => {
    const fetcher = vi.fn<typeof fetch>()
      .mockResolvedValueOnce(Response.json({
        items: [loop()],
        commitments: [promise(1)],
        truncated: false,
        skipped_unparseable: 0,
        coverage_incomplete: true,
      }))
      .mockRejectedValueOnce(new Error("temporary failure"))
      .mockResolvedValueOnce(Response.json({
        items: [loop(), loop(43, "awaiting_reply", "Can you review this?")],
        commitments: [promise(2)],
        truncated: false,
        skipped_unparseable: 0,
        coverage_incomplete: false,
      }));
    render(PersonOpenLoops, { client: createAPIClient(fetcher), personID: 7 });

    const button = await screen.findByRole("button", { name: "Check more" });
    expect(fetcher).toHaveBeenCalledTimes(1);
    expect(await screen.findByRole("link", { name: "“Promise 1”" })).toBeDefined();
    button.click();

    expect((await screen.findByRole("alert")).textContent).toContain(
      "Couldn't check more archive history.",
    );
    expect(screen.getByRole("link", { name: "Review the draft?" })).toBeDefined();
    expect(screen.getByRole("button", { name: "Check more" })).toBeDefined();
    button.click();

    expect(await screen.findByRole("link", { name: "Can you review this?" }))
      .toBeDefined();
    expect(await screen.findByRole("link", { name: "“Promise 2”" })).toBeDefined();
    expect(screen.queryByRole("link", { name: "“Promise 1”" })).toBeNull();
    expect(fetcher).toHaveBeenCalledTimes(3);
    expect(screen.queryByRole("button", { name: "Check more" })).toBeNull();
  });

  it("shows incomplete promise discovery instead of implying everything was checked", async () => {
    const respond = (body: object) => createAPIClient(vi.fn<typeof fetch>(async () =>
      Response.json({ items: [], truncated: false, skipped_unparseable: 0, coverage_incomplete: false, ...body })));
    const view = render(PersonOpenLoops, {
      client: respond({ commitments_complete: false, commitments_since: "2000-10-01T00:00:00Z" }),
      personID: 7,
    });
    expect(await screen.findByText("Promises since Oct 1, 2000 · still searching")).toBeDefined();
    expect(screen.queryByText("Nothing open with this person.")).toBeNull();
    view.unmount();

    const withLoops = render(PersonOpenLoops, {
      client: respond({ items: [loop()], commitments_complete: false, commitments_since: "2000-10-01T00:00:00Z", commitments_until: "2000-10-31T00:00:00Z" }),
      personID: 7,
    });
    expect(await screen.findByText("Promises from Oct 1, 2000 through Oct 31, 2000 · still searching")).toBeDefined();
    expect(screen.getByRole("link", { name: "Review the draft?" })).toBeDefined();
    withLoops.unmount();

    const notStarted = render(PersonOpenLoops, { client: respond({ commitments_complete: false }), personID: 7 });
    expect(await screen.findByText("Promise discovery has not started.")).toBeDefined();
    expect(screen.queryByText("Nothing open with this person.")).toBeNull();
    notStarted.unmount();
    render(PersonOpenLoops, { client: respond({
      commitments_complete: true,
      commitments_since: "2000-10-01T00:00:00Z", commitments_until: "2000-10-31T00:00:00Z",
    }), personID: 7 });
    expect(await screen.findByText("Promises from Oct 1, 2000 through Oct 31, 2000")).toBeDefined();
  });

  it("shows an error", async () => {
    const client = createAPIClient(
      vi.fn<typeof fetch>(async () =>
        Response.json(
          { error: "internal_error", message: "boom" },
          { status: 500 },
        ),
      ),
    );
    render(PersonOpenLoops, { client, personID: 7 });
    expect((await screen.findByRole("alert")).textContent).toContain(
      "Couldn't load open loops.",
    );
  });

  it("ignores a late response after selection changes", async () => {
    let resolveOld!: (response: Response | PromiseLike<Response>) => void;
    const old = new Promise<Response>((resolve) => {
      resolveOld = resolve;
    });
    const client = createAPIClient(
      vi.fn<typeof fetch>(async (input) => {
        const request = input instanceof Request ? input : new Request(input);
        return new URL(request.url).searchParams.get("person_id") === "7"
          ? old
          : Response.json({
              items: [],
              truncated: false,
              skipped_unparseable: 0,
              coverage_incomplete: false,
            });
      }),
    );
    const view = render(PersonOpenLoops, { client, personID: 7 });
    await view.rerender({ client, personID: 8 });
    expect(
      await screen.findByText("Nothing open with this person."),
    ).toBeDefined();
    const response = Response.json({
      items: [loop()],
      truncated: false,
      skipped_unparseable: 0,
      coverage_incomplete: false,
    });
    const readText = response.text.bind(response);
    const consumed = new Promise<void>((resolve) => {
      vi.spyOn(response, "text").mockImplementation(() => {
        const reading = readText();
        void reading.then(() => resolve());
        return reading;
      });
    });
    resolveOld(response);
    await consumed;
    await tick();
    expect(screen.queryByText("Review the draft?")).toBeNull();
  });
});
