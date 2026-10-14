// Package filemeta owns attachment metadata classification shared by archive
// authority and analytical views.
package filemeta

// MIMEFamilySQL preserves the existing exact MIME groups. It deliberately
// neither trims whitespace nor strips parameters. expression is trusted SQL.
func MIMEFamilySQL(expression string) string {
	mime := "lower(" + expression + ")"
	return `CASE
			WHEN ` + mime + ` LIKE 'image/%' THEN 'image'
			WHEN ` + mime + ` = 'application/pdf' THEN 'pdf'
			WHEN ` + mime + ` LIKE 'audio/%' THEN 'audio'
			WHEN ` + mime + ` LIKE 'video/%' THEN 'video'
			WHEN ` + mime + ` LIKE 'text/%' THEN 'text'
			WHEN ` + mime + ` IN ('application/msword', 'application/rtf',
				'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
				'application/vnd.oasis.opendocument.text') THEN 'document'
			WHEN ` + mime + ` IN ('application/zip', 'application/gzip', 'application/x-tar',
				'application/x-7z-compressed', 'application/x-rar-compressed') THEN 'archive'
			ELSE 'other'
		END`
}
