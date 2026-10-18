package store

import "go.kenn.io/msgvault/internal/mime"

type MessageAutomationSignals struct {
	AutoSubmitted, PrecedenceBulk, ListUnsubscribe, CalendarPart, ReplyToNoReply bool
	ParserVersion                                                                int
}

func AutomationSignalsFromMIME(in mime.AutomationSignals) MessageAutomationSignals {
	return MessageAutomationSignals{AutoSubmitted: in.AutoSubmitted, PrecedenceBulk: in.PrecedenceBulk, ListUnsubscribe: in.ListUnsubscribe, CalendarPart: in.CalendarPart, ReplyToNoReply: in.ReplyToNoReply, ParserVersion: mime.AutomationParserVersion}
}

func persistMessageAutomationSignalsWith(q querier, messageID int64, signals MessageAutomationSignals) error {
	_, err := q.Exec(`INSERT INTO message_automation_signals(message_id,auto_submitted,precedence_bulk,list_unsubscribe,calendar_part,reply_to_noreply,parser_version) VALUES(?,?,?,?,?,?,?) ON CONFLICT(message_id) DO UPDATE SET auto_submitted=excluded.auto_submitted,precedence_bulk=excluded.precedence_bulk,list_unsubscribe=excluded.list_unsubscribe,calendar_part=excluded.calendar_part,reply_to_noreply=excluded.reply_to_noreply,parser_version=excluded.parser_version`, messageID, signals.AutoSubmitted, signals.PrecedenceBulk, signals.ListUnsubscribe, signals.CalendarPart, signals.ReplyToNoReply, signals.ParserVersion)
	return err
}
