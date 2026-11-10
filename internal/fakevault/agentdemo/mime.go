package agentdemo

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"mime/multipart"
	"net/textproto"
	"time"
)

func contractV2Bytes() []byte {
	return []byte("%PDF-1.4\n% Synthetic contract version 2.\n1 0 obj << /Type /Catalog >> endobj\n%%EOF\n")
}

type emailMessage struct {
	handle, thread, from, to, subject, body, reply, extra string
	at                                                    time.Time
	labels                                                []string
	attachment                                            bool
}

func rawMIME(m emailMessage) ([]byte, error) {
	var out bytes.Buffer
	fmt.Fprintf(&out, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@demo.example>\r\nMIME-Version: 1.0\r\n", m.from, m.to, m.subject, m.at.Format(time.RFC1123Z), m.handle)
	if m.reply != "" {
		fmt.Fprintf(&out, "In-Reply-To: <%s@demo.example>\r\nReferences: <%s@demo.example>\r\n", m.reply, m.reply)
	}
	out.WriteString(m.extra)
	if !m.attachment {
		out.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
		out.WriteString(m.body)
		return out.Bytes(), nil
	}
	writer := multipart.NewWriter(&out)
	if err := writer.SetBoundary("agent-demo-contract-v2"); err != nil {
		return nil, err
	}
	fmt.Fprintf(&out, "Content-Type: multipart/mixed; boundary=%q\r\n\r\n", writer.Boundary())
	part, err := writer.CreatePart(textproto.MIMEHeader{"Content-Type": {"text/plain; charset=utf-8"}})
	if err != nil {
		return nil, err
	}
	if _, err = part.Write([]byte(m.body)); err != nil {
		return nil, err
	}
	part, err = writer.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/pdf"}, "Content-Disposition": {"attachment; filename=\"contract-v2.pdf\""}, "Content-Transfer-Encoding": {"base64"}})
	if err != nil {
		return nil, err
	}
	if _, err = part.Write([]byte(base64.StdEncoding.EncodeToString(contractV2Bytes()))); err != nil {
		return nil, err
	}
	if err = writer.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
