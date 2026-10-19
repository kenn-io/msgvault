import { render, screen } from "@testing-library/svelte";
import { describe, expect, it, vi } from "vitest";
import { tick } from "svelte";
import { createAPIClient } from "../../api/client";
import PersonOpenLoops from "./PersonOpenLoops.svelte";

function loop(id = 12) {
  return {
    id,
    kind: "reply_owed",
    summary: id === 12 ? "Review the draft?" : `Review item ${id}?`,
    age_seconds: 172800,
    anchor_message_id: id + 30,
    conversation_id: id + 65,
    owed_since_at: "2026-10-15T12:00:00Z",
  };
}

function clientFor(
  items: ReturnType<typeof loop>[],
  stale = false,
  nextCursor = "",
) {
  return createAPIClient(
    vi.fn<typeof fetch>(async () =>
      Response.json({
        items,
        next_cursor: nextCursor,
        freshness: { stale },
      }),
    ),
  );
}

describe("PersonOpenLoops", () => {
  it("shows current loop evidence and a reader link without edit controls", async () => {
    render(PersonOpenLoops, { client: clientFor([loop()]), personID: 7 });
    expect(screen.getByRole("heading", { name: "Open loops" })).toBeDefined();
    expect(
      await screen.findByRole("link", { name: "Review the draft?" }),
    ).toBeDefined();
    expect(
      screen
        .getByRole("link", { name: "Review the draft?" })
        .getAttribute("href"),
    ).toBe("/messages/42");
    expect(screen.getByText("Waiting on you")).toBeDefined();
    expect(screen.getByText("2d")).toBeDefined();
    expect(screen.getByText("1 open loop")).toBeDefined();
    expect(
      screen.queryByRole("button", { name: /done|snooze|mute/i }),
    ).toBeNull();
  });

  it("retains stale results with a visible warning", async () => {
    render(PersonOpenLoops, {
      client: clientFor(
        [loop(12), loop(13), loop(14), loop(15), loop(16)],
        true,
        "more",
      ),
      personID: 7,
    });
    expect(await screen.findByRole("status")).toBeDefined();
    expect(screen.getByRole("status").textContent).toContain(
      "Archive may be stale",
    );
    expect(screen.getByText("5+ open loops")).toBeDefined();
    expect(
      screen.getByRole("link", { name: "Review the draft?" }),
    ).toBeDefined();
  });

  it("shows an empty state", async () => {
    render(PersonOpenLoops, { client: clientFor([]), personID: 7 });
    expect(
      await screen.findByText("No open loops with this person."),
    ).toBeDefined();
  });

  it("clears old results and ignores a late response after selection changes", async () => {
    let resolveOld!: (response: Response | PromiseLike<Response>) => void;
    const old = new Promise<Response>((resolve) => {
      resolveOld = resolve;
    });
    const client = createAPIClient(
      vi.fn<typeof fetch>(async (input) => {
        const request = input instanceof Request ? input : new Request(input);
        return new URL(request.url).pathname.includes("/7/")
          ? old
          : Response.json({ items: [], freshness: { stale: false } });
      }),
    );
    const view = render(PersonOpenLoops, { client, personID: 7 });
    await view.rerender({ client, personID: 8 });
    expect(
      await screen.findByText("No open loops with this person."),
    ).toBeDefined();
    resolveOld(Response.json({ items: [loop()], freshness: { stale: true } }));
    await old;
    // Let HTTP decoding finish before checking for a late DOM update.
    await new Promise((resolve) => setTimeout(resolve, 0));
    await tick();
    expect(screen.queryByText("Review the draft?")).toBeNull();
  });
});
