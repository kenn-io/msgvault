package query

// EmptyMessageRecordsSQL defines the derived Parquet schema. SQLite stores no
// extracted records; builders and schema-only cache fixtures share this shape.
const EmptyMessageRecordsSQL = `SELECT
	NULL::BIGINT AS message_id, NULL::VARCHAR AS kind, NULL::VARCHAR AS basis,
	NULL::VARCHAR AS merchant, NULL::VARCHAR AS order_number,
	NULL::DECIMAL(22,4) AS amount, NULL::VARCHAR AS currency,
	NULL::VARCHAR AS tracking_number, NULL::VARCHAR AS carrier,
	NULL::VARCHAR AS status, NULL::TIMESTAMPTZ AS starts_at,
	NULL::TIMESTAMPTZ AS ends_at, NULL::VARCHAR AS origin,
	NULL::VARCHAR AS destination, NULL::VARCHAR AS confirmation_code,
	NULL::INTEGER AS dropped, NULL::VARCHAR AS extractor_version,
	NULL::JSON AS extra WHERE FALSE`
