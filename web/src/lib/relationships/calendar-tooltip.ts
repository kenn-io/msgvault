import type { RelationshipCalendarDay } from './models';

const dateFormat = new Intl.DateTimeFormat('en-US', {
  month: 'short', day: 'numeric', year: 'numeric', timeZone: 'UTC'
});

export function dayTooltipText(day: RelationshipCalendarDay): string {
  const when = dateFormat.format(new Date(`${day.date}T00:00:00Z`));
  const messages = day.email + day.chat;
  const head = messages === 0
    ? `No messages on ${when}`
    : `${messages} message${messages === 1 ? '' : 's'} on ${when}`;
  return day.meetings > 0
    ? `${head}, ${day.meetings} meeting${day.meetings === 1 ? '' : 's'}`
    : head;
}
