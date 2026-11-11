import { fireEvent, render, screen, within } from "@testing-library/svelte";
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

function commitmentClient() {
  let state: "open" | "done" | "snoozed" = "open";
  const decisions: { path: string; state: string }[] = [];
  const promise = (id: number, direction: 'owed_by' | 'owed_to', text: string, current: "open" | "done" | "snoozed" | "fulfilled" = "open") => ({
    id: `c${id}`,
    person_id: 7,
    claim_id: id + 100,
    direction,
    summary: text,
    promise: { message_id: id + 40, text, start: 0, end: text.length },
    promised_at: "2026-10-12T12:00:00Z",
    due_on: id === 12 ? "2026-11-15" : null,
    expires_at: "2026-12-15T00:00:00Z",
    state: current,
  });
  const fetcher = vi.fn<typeof fetch>(async (request, init) => {
    const url = new URL(request instanceof Request ? request.url : String(request), "http://localhost");
    if (url.pathname.endsWith("/decision")) {
      const body = request instanceof Request ? await request.json() : JSON.parse(String(init?.body));
      decisions.push({ path: url.pathname, state: body.state });
      state = body.state;
      return Response.json({ id: "c12", state });
    }
    return Response.json({
      items: [],
      commitments: [
        ...(state === "open" || url.searchParams.get("include_closed") === "true"
          ? [promise(12, "owed_by", "I will send the draft.", state)] : []),
        promise(13, "owed_to", "I will check the venue."),
        ...(url.searchParams.get("include_closed") === "true" ? [{
          ...promise(14, "owed_by", "I will send the agenda.", "fulfilled"),
          proposed_fulfilment: { message_id: 55, text: "I sent the agenda.", start: 0, end: 18 },
        }] : []),
      ],
      truncated: false,
      coverage_incomplete: false,
      skipped_unparseable: 0,
    });
  });
  return { client: createAPIClient(fetcher), decisions };
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
    expect(screen.getAllByText("2d")).toHaveLength(2);
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

  it.each(["done", "snoozed"] as const)("hides a %s commitment until closed promises are shown", async (closeState) => {
    const { client, decisions } = commitmentClient();
    const view = render(PersonOpenLoops, { client, personID: 7 });
    const cite = await screen.findByRole("link", { name: "I will send the draft." });
    expect(cite.getAttribute("href")).toBe("/messages/52");
    expect(screen.getByRole("heading", { name: "Promises you made" })).toBeDefined();
    expect(screen.getByRole("heading", { name: "Promises to you" })).toBeDefined();
    await fireEvent.click(screen.getAllByRole("button", { name: closeState === "done" ? "Done" : "Snooze for 24h" })[0]);
    expect(await screen.findByText("1 open")).toBeDefined();
    expect(screen.queryByRole("link", { name: "I will send the draft." })).toBeNull();
    view.unmount();
    render(PersonOpenLoops, { client, personID: 7 });
    expect(await screen.findByText("1 open")).toBeDefined();
    await fireEvent.click(screen.getByRole("button", { name: "Show closed promises" }));
    const restored = await screen.findByRole("link", { name: "I will send the draft." });
    await fireEvent.click(within(restored.closest("li")!).getByRole("button", { name: "Reopen" }));
    expect(await screen.findByText("2 open")).toBeDefined();
    expect(decisions).toEqual([
      { path: "/api/v1/loops/commitments/c12/decision", state: closeState },
      { path: "/api/v1/loops/commitments/c12/decision", state: "open" },
    ]);
  });

  it("shows exact promise text and labels fulfilment once", async () => {
    const { client } = commitmentClient();
    render(PersonOpenLoops, { client, personID: 7 });
    await screen.findByRole("link", { name: "I will send the draft." });
    expect(screen.getAllByText("I will send the draft.")).toHaveLength(1);
    expect(screen.getByText("Due 2026-11-15")).toBeDefined();
    expect(screen.getAllByText("I will check the venue.")).toHaveLength(1);
    await fireEvent.click(screen.getByRole("button", { name: "Show closed promises" }));
    expect(await screen.findByRole("link", { name: "Fulfilment · I sent the agenda." })).toBeDefined();
    expect(screen.queryByText("Fulfilled")).toBeNull();
  });

  it("labels truncated open-only and closed-inclusive counts accurately", async () => {
    const { client, fetcher } = clientFor(
      [loop(1), loop(2), loop(3), loop(4), loop(5)],
      true,
    );
    render(PersonOpenLoops, { client, personID: 7 });
    expect(await screen.findByText("5+ open")).toBeDefined();
    fetcher.mockResolvedValueOnce(Response.json({
      items: [loop(1)],
      commitments: Array.from({ length: 5 }, (_, index) => ({ id: `c${index + 7}`, person_id: 7, claim_id: index + 107, direction: "owed_by", state: "done",
        summary: "I will send the report.", promise: { message_id: 47, text: "I will send the report.", start: 0, end: 23 },
        promised_at: "2026-10-12T12:00:00Z", due_on: null, expires_at: "2026-12-15T00:00:00Z" })),
      truncated: true, coverage_incomplete: false, skipped_unparseable: 0,
    }));
    await fireEvent.click(screen.getByRole("button", { name: "Show closed promises" }));
    expect(await screen.findByText("1 open shown")).toBeDefined();
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
    expect(await screen.findByText("No loops found in the messages checked."))
      .toBeDefined();
  });

  it("checks more archive history when coverage is incomplete", async () => {
    const fetcher = vi.fn<typeof fetch>()
      .mockResolvedValueOnce(Response.json({
        items: [loop()],
        truncated: false,
        skipped_unparseable: 0,
        coverage_incomplete: true,
      }))
      .mockRejectedValueOnce(new Error("temporary failure"))
      .mockResolvedValueOnce(Response.json({
        items: [loop(), loop(43, "awaiting_reply", "Can you review this?")],
        truncated: false,
        skipped_unparseable: 0,
        coverage_incomplete: false,
      }));
    render(PersonOpenLoops, { client: createAPIClient(fetcher), personID: 7 });

    const button = await screen.findByRole("button", { name: "Check more" });
    expect(fetcher).toHaveBeenCalledTimes(1);
    button.click();

    expect((await screen.findByRole("alert")).textContent).toContain(
      "Couldn't check more archive history.",
    );
    expect(screen.getByRole("link", { name: "Review the draft?" })).toBeDefined();
    expect(screen.getByRole("button", { name: "Check more" })).toBeDefined();
    button.click();

    expect(await screen.findByRole("link", { name: "Can you review this?" }))
      .toBeDefined();
    expect(fetcher).toHaveBeenCalledTimes(3);
    expect(screen.queryByRole("button", { name: "Check more" })).toBeNull();
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
