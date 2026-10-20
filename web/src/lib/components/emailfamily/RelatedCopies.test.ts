import { fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { afterEach, describe, expect, it, vi } from "vitest";
import { createAPIClient } from "../../api/client";
import RelatedCopies from "./RelatedCopies.svelte";

afterEach(() => vi.restoreAllMocks());

const member = (id: number) => ({
  message_id: id,
  source_id: id,
  source_type: "imap",
  source_identifier: `account-${id}@example.test`,
  account_address: "",
  subject: `Copy ${id}`,
  linkage: { outcome: "complete" },
  labels: [],
  evidence_kinds: ["self"],
});
const family = (ids = [1, 2], revision = "before") => ({
  anchor_message_id: 1,
  members: ids.map(member),
  handling: { state: "unknown", reasons: ["provider_evidence_unavailable"] },
  truncated: false,
  revision,
  decisions: [],
});

describe("RelatedCopies", () => {
  it("downloads the family ZIP through the generated API operation", async () => {
    const blob = new Blob(["PK synthetic archive"], {
      type: "application/zip",
    });
    const requests: { path: string; method: string; body?: unknown }[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      const body = request.method === "POST" ? await request.clone().json() : undefined;
      requests.push({ path, method: request.method, body });
      return path.endsWith("/family/export")
        ? new Response(blob)
        : Response.json(family());
    });
    const createObjectURL = vi
      .spyOn(URL, "createObjectURL")
      .mockReturnValue("blob:family-export");
    const anchorClick = vi
      .spyOn(HTMLAnchorElement.prototype, "click")
      .mockImplementation(() => undefined);
    const client = createAPIClient(fetchFn);
    render(RelatedCopies, { props: { client, messageID: 1 } });

    await fireEvent.click(await screen.findByText("Related copies (2)"));
    await fireEvent.click(
      screen.getByRole("button", { name: "Download originals ZIP" }),
    );

    await waitFor(() =>
      expect(requests).toContainEqual({
        path: "/api/v1/messages/1/family/export",
        method: "POST",
        body: { expected_family_revision: "before" },
      }),
    );
    await waitFor(() => expect(anchorClick).toHaveBeenCalledOnce());
    expect(createObjectURL).toHaveBeenCalledWith(blob);
  });

  it("refreshes the family snapshot after a stale ZIP export", async () => {
    const blob = new Blob(["PK synthetic archive"], {
      type: "application/zip",
    });
    const exportRevisions: string[] = [];
    let familyReads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith("/family/export")) {
        const body = (await request.clone().json()) as {
          expected_family_revision: string;
        };
        exportRevisions.push(body.expected_family_revision);
        return exportRevisions.length === 1
          ? Response.json(
              { error: "family_changed", message: "Family evidence changed" },
              { status: 409 },
            )
          : new Response(blob);
      }
      if (path.endsWith("/family")) {
        familyReads++;
        return Response.json(
          family([1, 2], familyReads === 1 ? "before" : "after"),
        );
      }
      throw new Error(`Unexpected request: ${request.method} ${path}`);
    });
    const createObjectURL = vi
      .spyOn(URL, "createObjectURL")
      .mockReturnValue("blob:family-export");
    const anchorClick = vi
      .spyOn(HTMLAnchorElement.prototype, "click")
      .mockImplementation(() => undefined);
    const client = createAPIClient(fetchFn);
    render(RelatedCopies, { props: { client, messageID: 1 } });

    await fireEvent.click(await screen.findByText("Related copies (2)"));
    await fireEvent.click(
      screen.getByRole("button", { name: "Download originals ZIP" }),
    );

    await waitFor(() => expect(familyReads).toBe(2));
    await fireEvent.click(
      screen.getByRole("button", { name: "Download originals ZIP" }),
    );
    await waitFor(() => expect(exportRevisions).toEqual(["before", "after"]));
    await waitFor(() => expect(anchorClick).toHaveBeenCalledOnce());
    expect(createObjectURL).toHaveBeenCalledWith(blob);
  });

  it("keeps manual linking available for a complete singleton", async () => {
    const client = createAPIClient(
      vi.fn(async () => Response.json(family([1]))),
    );
    render(RelatedCopies, { props: { client, messageID: 1 } });
    await screen.findByRole("button", { name: "Link related copies" });
    expect(screen.queryByText(/Related copies \(/)).toBeNull();
    await fireEvent.click(
      screen.getByRole("button", { name: "Link related copies" }),
    );
    expect(screen.getByLabelText("Other archive message IDs")).toBeTruthy();
  });

  it("previews exact selected copies before confirming a write, then offers undo", async () => {
    const calls: { path: string; body: Record<string, unknown> }[] = [];
    const client = createAPIClient(
      vi.fn(async (input) => {
        const request = input as Request;
        const path = new URL(request.url).pathname;
        const body = request.method === "POST" ? await request.json() : {};
        calls.push({ path, body });
        if (path.endsWith("/preview"))
          return Response.json({
            before_revision: "before",
            family: {
              ...family(),
              handling: { state: "handled", reasons: [] },
            },
          });
        if (request.method === "POST")
          return Response.json({
            decision: {
              id: 9,
              revision: 1,
              kind: "handled",
              state: "active",
              message_ids: [2],
            },
            family: { ...family([2]), anchor_message_id: 2, revision: "after" },
          });
        return Response.json(family());
      }),
    );
    render(RelatedCopies, { props: { client, messageID: 1 } });
    await fireEvent.click(await screen.findByText("Related copies (2)"));
    await fireEvent.click(
      screen.getByRole("checkbox", { name: "Select copy 2" }),
    );
    await fireEvent.click(
      screen.getByRole("button", { name: "Preview handled" }),
    );
    await screen.findByRole("button", { name: "Confirm decision" });
    expect(calls.filter((c) => c.path.endsWith("/decisions"))).toHaveLength(0);
    expect(
      calls.find((c) => c.path.endsWith("/preview"))?.body.message_ids,
    ).toEqual([2]);
    await fireEvent.click(
      screen.getByRole("button", { name: "Confirm decision" }),
    );
    await screen.findByRole("button", { name: "Undo last decision" });
    await waitFor(() =>
      expect(
        calls.find((c) => c.path.endsWith("/decisions"))?.body
          .expected_family_revision,
      ).toBe("before"),
    );
    await waitFor(() =>
      expect(
        screen.getByRole("checkbox", { name: "Select copy 1" }),
      ).toBeTruthy(),
    );
  });
});

it("links IDs outside a singleton and discards a stale preview", async () => {
  const calls: Record<string, unknown>[] = [];
  const client = createAPIClient(
    vi.fn(async (input) => {
      const request = input as Request;
      if (request.method === "GET") return Response.json(family([1]));
      const body = await request.json();
      calls.push(body);
      if (new URL(request.url).pathname.endsWith("/preview"))
        return Response.json({
          before_revision: "before",
          family: family([1, 99]),
        });
      return Response.json(
        {
          error: "family_changed",
          message: "Family evidence changed; preview again",
        },
        { status: 409 },
      );
    }),
  );
  render(RelatedCopies, { props: { client, messageID: 1 } });
  await fireEvent.click(
    await screen.findByRole("button", { name: "Link related copies" }),
  );
  await fireEvent.input(screen.getByLabelText("Other archive message IDs"), {
    target: { value: "99" },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Preview link" }));
  await fireEvent.click(
    await screen.findByRole("button", { name: "Confirm decision" }),
  );
  await screen.findByRole("alert");
  expect(calls[0].message_ids).toEqual([1, 99]);
  expect(screen.queryByRole("button", { name: "Confirm decision" })).toBeNull();
});

it("keeps the same idempotency key when an interrupted confirmation is retried", async () => {
  const writes: Record<string, unknown>[] = [];
  const client = createAPIClient(
    vi.fn(async (input) => {
      const request = input as Request;
      const path = new URL(request.url).pathname;
      if (request.method === "GET") return Response.json(family());
      if (path.endsWith("/preview"))
        return Response.json({ before_revision: "before", family: family() });
      writes.push(await request.json());
      if (writes.length === 1) throw new Error("Connection interrupted");
      return Response.json({
        decision: {
          id: 9,
          revision: 1,
          kind: "handled",
          state: "active",
          message_ids: [2],
        },
        family: family(),
      });
    }),
  );
  render(RelatedCopies, { props: { client, messageID: 1 } });
  await fireEvent.click(await screen.findByText("Related copies (2)"));
  await fireEvent.click(
    screen.getByRole("checkbox", { name: "Select copy 2" }),
  );
  await fireEvent.click(
    screen.getByRole("button", { name: "Preview handled" }),
  );
  await fireEvent.click(
    await screen.findByRole("button", { name: "Confirm decision" }),
  );
  await screen.findByRole("alert");
  await fireEvent.click(
    screen.getByRole("button", { name: "Confirm decision" }),
  );
  await screen.findByRole("button", { name: "Undo last decision" });
  expect(writes).toHaveLength(2);
  expect(writes[0]).toEqual(writes[1]);
});

it("ignores an old read after navigation even if the transport ignores abort", async () => {
  let resolve!: (value: Response) => void;
  const old = new Promise<Response>((yes) => (resolve = yes));
  const client = createAPIClient(
    vi.fn(async (input) =>
      new URL((input as Request).url).pathname.includes("/1/")
        ? old
        : Response.json({ ...family([8]), anchor_message_id: 8 }),
    ),
  );
  const view = render(RelatedCopies, { props: { client, messageID: 1 } });
  await view.rerender({ client, messageID: 8 });
  await screen.findByRole("button", { name: "Link related copies" });
  resolve(Response.json(family()));
  await new Promise((yes) => setTimeout(yes, 0));
  expect(screen.queryByText("Related copies (2)")).toBeNull();
});

it.each([{ surviving: [1] }, { surviving: [] }])(
  "undoes using the current decision union after copies disappear: $surviving",
  async ({ surviving }) => {
    const calls: { path: string; body: Record<string, unknown> }[] = [];
    let familyReads = 0;
    const decision = {
      id: 9,
      revision: 1,
      kind: "link",
      state: "active",
      message_ids: [1, 2],
    };
    const client = createAPIClient(
      vi.fn(async (input) => {
        const request = input as Request;
        const path = new URL(request.url).pathname;
        const body = request.method === "POST" ? await request.json() : {};
        calls.push({ path, body });
        if (path.endsWith("/decisions/9"))
          return Response.json({
            decision: { ...decision, message_ids: surviving },
            family: {
              ...family(surviving),
              revision: "current-surviving-union",
            },
          });
        if (path.endsWith("/decisions")) return Response.json([decision]);
        if (path.endsWith("/undo"))
          return Response.json({
            decision: { ...decision, state: "undone" },
            family: family(surviving),
          });
        if (path.endsWith("/preview"))
          return Response.json(
            { error: "not_found", message: "Copy was deleted" },
            { status: 404 },
          );
        if (path.endsWith("/family")) {
          familyReads++;
          if (familyReads === 1) return Response.json(family());
          if (surviving.length === 0)
            return Response.json(
              { error: "not_found", message: "No copies remain" },
              { status: 404 },
            );
          return Response.json(family(surviving));
        }
        return Response.json(family());
      }),
    );
    render(RelatedCopies, { props: { client, messageID: 1 } });
    await fireEvent.click(await screen.findByText("Related copies (2)"));
    await fireEvent.click(
      screen.getByRole("button", { name: "Decision history" }),
    );
    await fireEvent.click(
      await screen.findByRole("button", { name: "Undo decision 9" }),
    );
    await waitFor(() =>
      expect(calls.find((c) => c.path.endsWith("/undo"))?.body).toEqual({
        expected_revision: 1,
        expected_family_revision: "current-surviving-union",
      }),
    );
    expect(calls.some((c) => c.path.endsWith("/preview"))).toBe(false);
    await waitFor(() => {
      if (surviving.length === 0) {
        expect(
          screen.queryByRole("button", { name: "Link related copies" }),
        ).toBeNull();
      } else {
        expect(
          screen.queryByRole("heading", { name: "Recent decisions" }),
        ).toBeNull();
      }
    });
    expect(screen.queryByRole("alert")).toBeNull();
  },
);

it.each(["history", "last"])(
  "requires fresh history after a stale %s undo conflicts",
  async (entry) => {
    const revisions: number[] = [];
    let historyReads = 0;
    const decision = {
      id: 9,
      revision: 1,
      kind: "handled",
      state: "active",
      message_ids: [2],
    };
    const client = createAPIClient(
      vi.fn(async (input) => {
        const request = input as Request;
        const path = new URL(request.url).pathname;
        if (path.endsWith("/decisions/9"))
          return Response.json({
            decision: { ...decision, revision: 2 },
            family: family(),
          });
        if (path.endsWith("/undo")) {
          const body = await request.json();
          revisions.push(body.expected_revision);
          if (body.expected_revision === 1)
            return Response.json(
              {
                error: "family_changed",
                message: "Decision changed; reload history",
              },
              { status: 409 },
            );
          return Response.json({
            decision: { ...decision, revision: 3, state: "undone" },
            family: family(),
          });
        }
        if (path.endsWith("/decisions")) {
          if (request.method === "POST")
            return Response.json({ decision, family: family() });
          historyReads++;
          return Response.json([
            {
              ...decision,
              revision: entry === "last" || historyReads > 1 ? 2 : 1,
            },
          ]);
        }
        if (path.endsWith("/preview"))
          return Response.json({ before_revision: "before", family: family() });
        return Response.json(family());
      }),
    );
    render(RelatedCopies, { props: { client, messageID: 1 } });
    await fireEvent.click(await screen.findByText("Related copies (2)"));
    if (entry === "history") {
      await fireEvent.click(
        screen.getByRole("button", { name: "Decision history" }),
      );
    } else {
      await fireEvent.click(
        screen.getByRole("checkbox", { name: "Select copy 2" }),
      );
      await fireEvent.click(
        screen.getByRole("button", { name: "Preview handled" }),
      );
      await fireEvent.click(
        await screen.findByRole("button", { name: "Confirm decision" }),
      );
    }
    const undoName =
      entry === "history" ? "Undo decision 9" : "Undo last decision";
    await screen.findByRole("button", { name: undoName });
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: undoName }).hasAttribute("disabled"),
      ).toBe(false),
    );
    await fireEvent.click(screen.getByRole("button", { name: undoName }));
    await screen.findByRole("alert");
    expect(revisions).toEqual([1]);
    expect(screen.queryByRole("button", { name: /^Undo/ })).toBeNull();
    await fireEvent.click(
      screen.getByRole("button", { name: "Decision history" }),
    );
    await fireEvent.click(
      await screen.findByRole("button", { name: "Undo decision 9" }),
    );
    await waitFor(() => expect(revisions).toEqual([1, 2]));
    expect(screen.queryByRole("alert")).toBeNull();
  },
);
