// Package synctechstate classifies the numeric send states in Synctech backups.
package synctechstate

// RawFormat identifies the preserved Synctech provider record.
const RawFormat = "synctech_sms_xml_json"

// SMSWasSent excludes draft, outbox, failed, and queued records.
func SMSWasSent(kind int) bool { return kind != 3 && kind != 4 && kind != 5 && kind != 6 }

// MMSWasSent excludes draft and outbox records.
func MMSWasSent(box int) bool { return box != 3 && box != 4 }
