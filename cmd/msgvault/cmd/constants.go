package cmd

// Source-type identifiers stored in sources.source_type and matched against
// when dispatching sync/import logic per account kind.
const (
	sourceTypeGmail = "gmail"
	sourceTypeIMAP  = "imap"
	sourceTypeMbox  = "mbox"
)

// Analytics dataset / SQLite table names. These double as the Parquet
// subdirectory under analytics/ and the source table in build-cache and
// repair-encoding queries, and as stats/JSON field keys for the same entities.
const (
	tableMessages      = "messages"
	tableLabels        = "labels"
	tableAttachments   = "attachments"
	tableParticipants  = "participants"
	tableConversations = "conversations"
)

// outputFormatJSON is the JSON output mode: both the --json flag name and the
// "json" value accepted by --format.
const outputFormatJSON = "json"

// keyEmail is the map/log field key carrying an account or address email.
const keyEmail = "email"
