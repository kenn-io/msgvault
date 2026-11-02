package query

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"io"
	"net/mail"
	"net/textproto"
	"strings"

	"go.kenn.io/msgvault/internal/senderauth"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
)

const senderAuthPrefixBytes = 128 * 1024
const senderAuthHeaderBytes = 256 * 1024

type senderAuthPolicy struct{ pins map[int64]senderauth.TrustSet }

// ConfigureSenderAuth installs an immutable copy of archive-local trust pins.
// Call before publishing an engine to readers. Wrappers forward this optional
// capability explicitly, preserving the engine's other optional interfaces.
func ConfigureSenderAuth(engine Engine, pins map[int64]senderauth.TrustSet) {
	if setter, ok := engine.(interface {
		SetSenderAuthPins(pins map[int64]senderauth.TrustSet)
	}); ok {
		setter.SetSenderAuthPins(pins)
	}
}
func (e *SQLiteEngine) SetSenderAuthPins(pins map[int64]senderauth.TrustSet) {
	copyPins := make(map[int64]senderauth.TrustSet, len(pins))
	for id, trust := range pins {
		copyPins[id] = append(senderauth.TrustSet(nil), trust...)
	}
	e.senderAuthPolicy.Store(&senderAuthPolicy{pins: copyPins})
}
func (e *pgEngine) SetSenderAuthPins(pins map[int64]senderauth.TrustSet) {
	ConfigureSenderAuth(e.Engine, pins)
}
func (e *DuckDBEngine) SetSenderAuthPins(pins map[int64]senderauth.TrustSet) {
	if e.sqliteEngine != nil {
		e.sqliteEngine.SetSenderAuthPins(pins)
	}
}

// readSenderAuth reads only selected IDs and enforces source scope again. A
// bounded raw prefix prevents attachment-sized allocations on this path.
// Unavailable evidence is advisory and cannot turn a successful read into an error.
func (e *SQLiteEngine) readSenderAuth(ctx context.Context, ids []int64) map[int64]*senderauth.Verdict {
	out := make(map[int64]*senderauth.Verdict, len(ids))
	for _, id := range ids {
		out[id] = senderauth.UnknownVerdict("no_raw")
	}
	for start := 0; start < len(ids); start += messageSummaryIDChunk {
		batch := ids[start:min(start+messageSummaryIDChunk, len(ids))]
		placeholders := make([]string, len(batch))
		args := []any{senderAuthPrefixBytes}
		for i, id := range batch {
			placeholders[i] = "?"
			args = append(args, id)
		}
		conditions, args := appendSourceFilter([]string{"m.id IN (" + strings.Join(placeholders, ",") + ")", store.LiveMessagesWhere("m", false)}, args, "m.", nil, sourcescope.IDs(ctx))
		func() {
			rows, err := e.queryContext(ctx, `SELECT m.id, m.source_id, COALESCE(m.message_type,''), `+store.SentMessageSQL()+`,
   COALESCE(r.raw_format,''), COALESCE(r.compression,''), COALESCE(r.encryption_version,0), substr(r.raw_data,1,?),
   CASE WHEN m.sender_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM message_recipients mr WHERE mr.message_id=m.id AND mr.recipient_type='from') THEN 1 ELSE (SELECT COUNT(*) FROM message_recipients mr WHERE mr.message_id=m.id AND mr.recipient_type='from') END,
   COALESCE((SELECT p.email_address FROM message_recipients mr JOIN participants p ON p.id=mr.participant_id WHERE mr.message_id=m.id AND mr.recipient_type='from' ORDER BY mr.id LIMIT 1),(SELECT p.email_address FROM participants p WHERE p.id=m.sender_id),'')
   FROM messages m JOIN sources src ON src.id=m.source_id LEFT JOIN message_raw r ON r.message_id=m.id WHERE `+strings.Join(conditions, " AND "), args...)
			if err != nil {
				return
			}
			defer func() { _ = rows.Close() }()
			valid := true
			for rows.Next() {
				var id, source int64
				var kind, format, compression, displayFrom string
				var own bool
				var encryption, fromCount int
				var raw []byte
				if err := rows.Scan(&id, &source, &kind, &own, &format, &compression, &encryption, &raw, &fromCount, &displayFrom); err != nil {
					valid = false
					break
				}
				verdict := senderauth.UnknownVerdict("no_raw")
				switch {
				case kind != "" && kind != "email":
					verdict = &senderauth.Verdict{Kind: "none", Basis: "not_email"}
				case own:
					verdict = &senderauth.Verdict{Kind: "none", Basis: "owner_sent"}
				case format == "mime" && encryption == 0 && len(raw) > 0:
					var trust senderauth.TrustSet
					if policy := e.senderAuthPolicy.Load(); policy != nil {
						trust = policy.pins[source]
					}
					verdict = senderAuthFromRaw(raw, compression, displayFrom, fromCount, trust)
				}
				out[id] = verdict
			}
			if rows.Err() != nil {
				valid = false
			}
			if !valid {
				for _, id := range batch {
					out[id] = senderauth.UnknownVerdict("no_raw")
				}
			}
		}()
	}
	return out
}

func senderAuthHeaders(raw []byte, compression string) (textproto.MIMEHeader, senderauth.Basis) {
	var reader io.Reader = bytes.NewReader(raw)
	if compression == "zlib" {
		z, err := zlib.NewReader(reader)
		if err != nil {
			return nil, senderauth.Unparseable
		}
		defer func() { _ = z.Close() }()
		reader = z
	} else if compression != "" && compression != "none" {
		return nil, "no_raw"
	}
	// Read lines only until the header terminator; do not inflate the body.
	limited := bufio.NewReader(io.LimitReader(reader, senderAuthHeaderBytes))
	var block bytes.Buffer
	complete := false
	for block.Len() < senderAuthHeaderBytes {
		line, err := limited.ReadString('\n')
		block.WriteString(line)
		if line == "\r\n" || line == "\n" {
			complete = true
			break
		}
		if err != nil {
			break
		}
	}
	if !complete {
		return nil, senderauth.HeaderTruncated
	}
	headers, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(block.Bytes()))).ReadMIMEHeader()
	if err != nil {
		return nil, senderauth.Unparseable
	}
	return headers, ""
}

func senderAuthFromRaw(raw []byte, compression, displayed string, fromCount int, trust senderauth.TrustSet) *senderauth.Verdict {
	headers, basis := senderAuthHeaders(raw, compression)
	if basis != "" {
		return senderauth.UnknownVerdict(basis)
	}
	values := []senderauth.Header{}
	for _, v := range headers.Values("Authentication-Results") {
		values = append(values, senderauth.Header{Name: "Authentication-Results", Value: v})
	}
	rawFrom, err := mail.ParseAddressList(strings.Join(headers.Values("From"), ","))
	domain := ""
	if err == nil && len(rawFrom) == 1 && len(headers.Values("From")) == 1 && fromCount == 1 {
		actual, err := mail.ParseAddress(displayed)
		if err == nil && strings.EqualFold(actual.Address, rawFrom[0].Address) {
			if at := strings.LastIndexByte(actual.Address, '@'); at >= 0 {
				domain = strings.ToLower(actual.Address[at+1:])
			}
		}
	}
	result := senderauth.Parse(values, trust, domain, false)
	return &senderauth.Verdict{Kind: "email", SPF: result.SPF, DKIM: result.DKIM, DMARC: result.DMARC, Verified: result.Verified, VerifiedDomain: result.VerifiedDomain, AuthservID: result.AuthservID, Basis: result.Basis}
}
func (e *SQLiteEngine) fillSenderAuth(ctx context.Context, messages []MessageSummary) {
	ids := make([]int64, len(messages))
	for i, m := range messages {
		ids[i] = m.ID
	}
	results := e.readSenderAuth(ctx, ids)
	for i := range messages {
		messages[i].SenderAuth = SenderAuthForAddresses(results[messages[i].ID], []Address{{Email: messages[i].FromEmail}})
	}
}

// SenderAuthReader derives advisory metadata for already selected message IDs.
type SenderAuthReader interface {
	ReadSenderAuth(ctx context.Context, ids []int64) map[int64]*senderauth.Verdict
}

func (e *SQLiteEngine) ReadSenderAuth(ctx context.Context, ids []int64) map[int64]*senderauth.Verdict {
	return e.readSenderAuth(ctx, ids)
}
func (e *pgEngine) ReadSenderAuth(ctx context.Context, ids []int64) map[int64]*senderauth.Verdict {
	if reader, ok := e.Engine.(SenderAuthReader); ok {
		return reader.ReadSenderAuth(ctx, ids)
	}
	out := make(map[int64]*senderauth.Verdict, len(ids))
	for _, id := range ids {
		out[id] = senderauth.UnknownVerdict("no_raw")
	}
	return out
}
func (e *DuckDBEngine) ReadSenderAuth(ctx context.Context, ids []int64) map[int64]*senderauth.Verdict {
	if e.sqliteEngine != nil {
		return e.sqliteEngine.readSenderAuth(ctx, ids)
	}
	out := map[int64]*senderauth.Verdict{}
	for _, id := range ids {
		out[id] = senderauth.UnknownVerdict("no_raw")
	}
	return out
}

func (e *DuckDBEngine) fillSenderAuth(ctx context.Context, messages []MessageSummary) {
	ids := make([]int64, len(messages))
	for i, m := range messages {
		ids[i] = m.ID
	}
	auth := e.ReadSenderAuth(ctx, ids)
	for i := range messages {
		messages[i].SenderAuth = SenderAuthForAddresses(auth[messages[i].ID], []Address{{Email: messages[i].FromEmail}})
	}
}

// SenderAuthForAddresses binds receipt evidence to the From value actually
// displayed by the selected result, including results from a stale cache.
func SenderAuthForAddresses(verdict *senderauth.Verdict, from []Address) *senderauth.Verdict {
	if verdict == nil || !verdict.Verified {
		return verdict
	}
	matches := false
	if len(from) == 1 {
		address, err := mail.ParseAddress(from[0].Email)
		if err == nil {
			at := strings.LastIndexByte(address.Address, '@')
			matches = at >= 0 && strings.EqualFold(address.Address[at+1:], verdict.VerifiedDomain)
		}
	}
	if matches {
		return verdict
	}
	copyVerdict := *verdict
	copyVerdict.Verified = false
	copyVerdict.VerifiedDomain = ""
	copyVerdict.Basis = senderauth.FromMismatch
	return &copyVerdict
}
