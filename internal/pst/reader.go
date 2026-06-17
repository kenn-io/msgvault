package pst

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	charsets "github.com/emersion/go-message/charset"
	pstlib "github.com/mooijtech/go-pst/v6/pkg"
	"github.com/mooijtech/go-pst/v6/pkg/properties"
	"github.com/rotisserie/eris"
	"golang.org/x/text/encoding"
)

func init() {
	// Register extended charsets so go-pst can decode non-UTF-8 encoded PST files.
	pstlib.ExtendCharsets(func(name string, enc encoding.Encoding) {
		charsets.RegisterEncoding(name, enc)
	})
}

// windowsFiletimeToTime converts a Windows FILETIME value (100-nanosecond
// intervals since 1601-01-01 UTC) to a time.Time. Returns zero time if ft == 0.
func windowsFiletimeToTime(ft int64) time.Time {
	if ft <= 0 {
		return time.Time{}
	}
	// 11644473600 seconds between Windows epoch (1601-01-01) and Unix epoch (1970-01-01).
	const epochDiff int64 = 11644473600
	secs := ft/10_000_000 - epochDiff
	ns := (ft % 10_000_000) * 100
	return time.Unix(secs, ns).UTC()
}

// File wraps a PST file for reading.
type File struct {
	pstFile *pstlib.File
	closer  io.Closer
}

// Open opens a PST file at path for reading.
func Open(path string) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	pstFile, err := pstlib.New(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("parse pst: %w", err)
	}
	return &File{pstFile: pstFile, closer: f}, nil
}

// Close releases all resources held by the File.
func (f *File) Close() error {
	f.pstFile.Cleanup()
	return f.closer.Close()
}

// FolderEntry holds metadata about a PST folder.
type FolderEntry struct {
	Name     string
	Path     string // Full slash-separated path, e.g. "Personal Folders/Inbox/Archive"
	MsgCount int32
}

// MessageEntry holds the extracted email properties from a PST message.
type MessageEntry struct {
	// EntryID is the PST node identifier, used as part of the dedup key.
	EntryID string

	// FolderPath is the slash-separated folder path this message came from.
	FolderPath string

	// Subject is the message subject.
	Subject string

	// TransportMessageHeaders contains the original RFC 5322 headers as stored
	// by the PST, available for most internet-delivered messages. Empty for
	// drafts or messages sent via Exchange-only paths.
	TransportHeaders string

	// Body content.
	BodyText string
	BodyHTML string

	// Sender fields.
	SenderName        string
	SenderEmail       string
	SenderAddressType string // "SMTP" or "EX" (Exchange DN)

	// DisplayTo/Cc/Bcc are semicolon-separated display names from MAPI.
	// Email addresses must be parsed from TransportHeaders when available.
	DisplayTo  string
	DisplayCc  string
	DisplayBcc string

	// Threading headers.
	MessageID  string
	InReplyTo  string
	References string

	// Timestamps.
	SentAt       time.Time // ClientSubmitTime
	ReceivedAt   time.Time // MessageDeliveryTime
	CreationTime time.Time // PidTagCreationTime
}

// AttachmentEntry holds metadata and content for a PST attachment.
type AttachmentEntry struct {
	Filename  string
	MIMEType  string
	ContentID string
	Size      int32
	Content   []byte
}

// WalkFolderFunc is called for each folder during WalkFolders. The raw pstlib
// folder is provided so callers can iterate messages.
type WalkFolderFunc func(entry FolderEntry, folder *pstlib.Folder) error

// WalkFolders walks all folders in the PST file recursively, building
// slash-separated folder paths. Search folders are skipped automatically.
// Returns the first non-nil error returned by fn.
func (f *File) WalkFolders(fn WalkFolderFunc) error {
	rootFolder, err := f.pstFile.GetRootFolder()
	if err != nil {
		return fmt.Errorf("get root folder: %w", err)
	}
	return walkFoldersRecursive(&rootFolder, "", fn)
}

func walkFoldersRecursive(folder *pstlib.Folder, parentPath string, fn WalkFolderFunc) error {
	path := folder.Name
	if parentPath != "" {
		path = parentPath + "/" + folder.Name
	}

	entry := FolderEntry{
		Name:     folder.Name,
		Path:     path,
		MsgCount: folder.MessageCount,
	}

	if err := fn(entry, folder); err != nil {
		return err
	}

	if !folder.HasSubFolders {
		return nil
	}

	subFolders, err := folder.GetSubFolders()
	if err != nil {
		// Some PST variants (e.g. 32-bit) can fail to read sub-folder
		// metadata; log and continue rather than aborting the walk.
		return nil //nolint:nilerr // intentional: keep walking sibling folders
	}
	for i := range subFolders {
		if err := walkFoldersRecursive(&subFolders[i], path, fn); err != nil {
			return err
		}
	}
	return nil
}

// ExtractMessage extracts email properties from a pstlib.Message.
// Returns nil if the message is not an email (e.g. calendar, contact, task).
func ExtractMessage(msg *pstlib.Message, folderPath string) *MessageEntry {
	props, ok := msg.Properties.(*properties.Message)
	if !ok {
		return nil
	}

	subject := props.GetSubject()
	if subject == "" {
		subject = props.GetNormalizedSubject()
	}
	if subject == "" {
		subject = props.GetInternetSubject()
	}

	senderEmail := props.GetSenderEmailAddress()
	// Exchange Distinguished Names start with /O= — try to resolve to SMTP.
	if isExchangeDN(senderEmail) {
		if smtp := props.GetSmtpAddress(); smtp != "" {
			senderEmail = smtp
		} else {
			senderEmail = extractCN(senderEmail)
		}
	}

	return &MessageEntry{
		EntryID:           fmt.Sprintf("%d", msg.Identifier),
		FolderPath:        folderPath,
		Subject:           subject,
		TransportHeaders:  props.GetTransportMessageHeaders(),
		BodyText:          props.GetBody(),
		BodyHTML:          props.GetBodyHtml(),
		SenderName:        props.GetSenderName(),
		SenderEmail:       senderEmail,
		SenderAddressType: props.GetSenderAddressType(),
		DisplayTo:         props.GetDisplayTo(),
		DisplayCc:         props.GetDisplayCc(),
		DisplayBcc:        props.GetDisplayBcc(),
		MessageID:         props.GetInternetMessageId(),
		InReplyTo:         props.GetInReplyToId(),
		References:        props.GetInternetReferences(),
		SentAt:            windowsFiletimeToTime(props.GetClientSubmitTime()),
		ReceivedAt:        windowsFiletimeToTime(props.GetMessageDeliveryTime()),
		CreationTime:      windowsFiletimeToTime(props.GetCreationTime()),
	}
}

var errAttachmentTooLarge = errors.New("attachment exceeds size limit")

// limitWriter wraps an io.Writer and returns errAttachmentTooLarge once the
// remaining byte budget is exhausted.
type limitWriter struct {
	w         io.Writer
	remaining int64
}

func (lw *limitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > lw.remaining {
		return 0, errAttachmentTooLarge
	}
	n, err := lw.w.Write(p)
	lw.remaining -= int64(n)
	return n, err
}

// ReadAttachments reads all attachments from a pstlib.Message into memory.
// Returns an empty slice (not an error) when there are no attachments.
// Individual attachment read errors are returned as a non-nil error.
func ReadAttachments(msg *pstlib.Message, maxBytes int64) ([]AttachmentEntry, error) {
	iter, err := msg.GetAttachmentIterator()
	if eris.Is(err, pstlib.ErrAttachmentsNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get attachment iterator: %w", err)
	}

	var attachments []AttachmentEntry
	var totalBytes int64

	for iter.Next() {
		att := iter.Value()

		filename := att.GetAttachLongFilename()
		if filename == "" {
			filename = att.GetAttachFilename()
		}

		mimeType := att.GetAttachMimeTag()
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}

		// Pre-check using the reported size when available; the bounded writer below
		// enforces the limit unconditionally, covering the size==0 case.
		if maxBytes > 0 {
			estimatedSize := int64(att.GetAttachSize())
			if estimatedSize > 0 && totalBytes+estimatedSize > maxBytes {
				break
			}
		}

		// Stream attachment content through a bounded writer so a corrupted or
		// malicious PST reporting size 0 cannot exhaust memory.
		var buf bytes.Buffer
		w := io.Writer(&buf)
		if maxBytes > 0 {
			w = &limitWriter{w: &buf, remaining: maxBytes - totalBytes}
		}
		written, err := att.WriteTo(w)
		if err != nil {
			// ErrAttachmentTooLarge means we hit the cap; stop reading further attachments.
			if errors.Is(err, errAttachmentTooLarge) {
				break
			}
			return nil, fmt.Errorf("read attachment %q: %w", filename, err)
		}
		totalBytes += written

		attachments = append(attachments, AttachmentEntry{
			Filename:  filename,
			MIMEType:  mimeType,
			ContentID: att.GetAttachContentId(),
			Size:      att.GetAttachSize(),
			Content:   buf.Bytes(),
		})
	}

	if err := iter.Err(); err != nil {
		return attachments, fmt.Errorf("attachment iterator: %w", err)
	}
	return attachments, nil
}

// isExchangeDN reports whether s looks like an Exchange Distinguished Name.
func isExchangeDN(s string) bool {
	return strings.HasPrefix(s, "/O=") || strings.HasPrefix(s, "/o=")
}

// extractCN extracts the last CN= component from an Exchange DN.
// E.g. "/O=CORP/OU=EXCHANGE/CN=RECIPIENTS/CN=JSMITH" → "JSMITH".
func extractCN(dn string) string {
	parts := strings.Split(dn, "/")
	for _, v := range slices.Backward(parts) {
		p := v
		if upper := strings.ToUpper(p); strings.HasPrefix(upper, "CN=") {
			return p[3:]
		}
	}
	return dn
}
