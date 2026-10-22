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
    expect(screen.getByRole("status").textContent).toContain(
      "Loading open loops",
    );
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
      await screen.findByText("Nothing open with this person in the last 30 days."),
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
    expect(
      await screen.findByText(
        "Only 1,000 stored messages were checked; more loops may be missing.",
      ),
    ).toBeDefined();
    view.unmount();

    const empty = clientFor([], false, 0, true);
    render(PersonOpenLoops, { client: empty.client, personID: 7 });
    expect(await screen.findByText("No loops found in the messages checked."))
      .toBeDefined();
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
    expect((await screen.findByRole("alert")).textContent).toBe(
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
      await screen.findByText("Nothing open with this person in the last 30 days."),
    ).toBeDefined();
    resolveOld(
      Response.json({
        items: [loop()],
        truncated: false,
        skipped_unparseable: 0,
        coverage_incomplete: false,
      }),
    );
    await old;
    // Let HTTP decoding finish before checking for a late DOM update.
    await new Promise((resolve) => setTimeout(resolve, 0));
    await tick();
    expect(screen.queryByText("Review the draft?")).toBeNull();
  });
});
