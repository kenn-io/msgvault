import type { EntryRow, ExploreFilter } from "./models";

export const MAILBOX_STATE_FILTERS = [
  {
    dimension: "inbox_state",
    label: "Provider Inbox",
    options: [
      { value: "inbox", label: "In Inbox" },
      { value: "outside", label: "Outside Inbox" },
      { value: "unknown", label: "Inbox unknown" },
    ],
  },
  {
    dimension: "read_state",
    label: "Provider read status",
    options: [
      { value: "unread", label: "Unread at provider" },
      { value: "read", label: "Read at provider" },
      { value: "unknown", label: "Read status unknown" },
    ],
  },
] as const;

export function mailboxStateLabel(dimension: string, value: string): string {
  return (
    MAILBOX_STATE_FILTERS.find(
      (filter) => filter.dimension === dimension,
    )?.options.find((option) => option.value === value)?.label ?? value
  );
}

export function providerMailboxStatus(
  row: EntryRow,
  filters: readonly ExploreFilter[] = [],
): string {
  if (
    row.kind !== "email" ||
    !["", "gmail", "imap", "msmail"].includes(row.source_type)
  )
    return "";
  const inboxState = row.inbox_state ?? "unknown";
  const readState = row.read_state ?? "unknown";
  const isSingleValueFilter = (dimension: string, value: string) => {
    const filter = filters.find((candidate) => candidate.dimension === dimension);
    return filter?.values.length === 1 && filter.values[0] === value;
  };
  const labels = [
    !isSingleValueFilter("inbox_state", inboxState)
      ? mailboxStateLabel("inbox_state", inboxState)
      : "",
    !isSingleValueFilter("read_state", readState)
      ? mailboxStateLabel("read_state", readState)
      : "",
  ].filter(Boolean);
  return labels.join(" · ");
}
