import type { EntryRow } from "./models";

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

export function providerMailboxStatus(row: EntryRow): string {
  if (
    row.kind !== "email" ||
    !["", "gmail", "imap", "msmail"].includes(row.source_type)
  )
    return "";
  return [
    mailboxStateLabel("inbox_state", row.inbox_state ?? "unknown"),
    mailboxStateLabel("read_state", row.read_state ?? "unknown"),
  ].join(" · ");
}
