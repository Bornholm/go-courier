package mail

import (
	"bytes"
	"io"
	"log/slog"
	"mime"
	"strings"

	"github.com/DusanKasan/parsemail"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/charset"
	"github.com/pkg/errors"
)

// parseEmail reads a raw message. parsemail reads the header; the MIME tree is
// walked with go-message, because parsemail leaves the transfer encoding and
// the charset of a single-part body undecoded, turns text attachments into
// body text, empties attachments declaring no transfer encoding and refuses
// the message when one declares quoted-printable or 8bit.
//
// A part that cannot be fully decoded keeps what could be read: a damaged
// message is better partly read than lost.
func parseEmail(r io.Reader) (parsemail.Email, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return parsemail.Email{}, errors.WithStack(err)
	}

	email, err := parsemail.Parse(bytes.NewReader(headerOnly(raw)))
	if err != nil {
		return parsemail.Email{}, errors.WithStack(err)
	}

	entity, err := message.Read(bytes.NewReader(raw))
	if entity == nil {
		return email, errors.WithStack(err)
	}

	var text, html []string

	err = entity.Walk(func(_ []int, part *message.Entity, err error) error {
		if err != nil && !message.IsUnknownEncoding(err) && !message.IsUnknownCharset(err) {
			return err
		}
		if part.MultipartReader() != nil {
			return nil
		}

		content, err := io.ReadAll(part.Body)
		if err != nil {
			slog.Warn("could not fully decode a message part", slog.Any("error", errors.WithStack(err)))
		}

		contentType, _, _ := part.Header.ContentType()
		if !strings.Contains(contentType, "/") { // unreadable: the default of RFC 2045
			contentType = "text/plain"
		}
		disposition, _, _ := part.Header.ContentDisposition()
		filename := partFilename(part.Header)
		cid := strings.Trim(part.Header.Get("Content-Id"), "<> ")

		switch {
		case disposition != "attachment" && filename == "" && contentType == "text/plain":
			text = append(text, strings.TrimRight(string(content), "\r\n"))
		case disposition != "attachment" && filename == "" && contentType == "text/html":
			html = append(html, strings.TrimRight(string(content), "\r\n"))
		case disposition != "attachment" && cid != "":
			email.EmbeddedFiles = append(email.EmbeddedFiles, parsemail.EmbeddedFile{
				CID: cid, ContentType: contentType, Data: bytes.NewReader(content),
			})
		default:
			email.Attachments = append(email.Attachments, parsemail.Attachment{
				Filename: filename, ContentType: contentType, Data: bytes.NewReader(content),
			})
		}

		return nil
	})
	if err != nil {
		slog.Warn("could not fully read a message", slog.Any("error", errors.WithStack(err)))
	}

	email.TextBody = strings.Join(text, "\n")
	email.HTMLBody = strings.Join(html, "\n")

	return email, nil
}

// headerOnly keeps the raw header, without its Content-Type so that parsemail
// does not look for parts in the empty body it is given.
func headerOnly(raw []byte) []byte {
	var out bytes.Buffer

	skipping := false
	for len(raw) > 0 {
		line := raw
		if i := bytes.IndexByte(raw, '\n'); i >= 0 {
			line = raw[:i+1]
		}
		raw = raw[len(line):]

		if len(bytes.TrimRight(line, "\r\n")) == 0 {
			break
		}

		if line[0] != ' ' && line[0] != '\t' {
			name, _, _ := bytes.Cut(line, []byte(":"))
			skipping = strings.EqualFold(string(bytes.TrimSpace(name)), "Content-Type")
		}
		if !skipping {
			out.Write(line)
		}
	}
	out.WriteString("\r\n")

	return out.Bytes()
}

// partFilename returns the decoded name of a part, from its Content-Disposition
// or, failing that, from its Content-Type.
func partFilename(header message.Header) string {
	_, params, _ := header.ContentDisposition()

	name := params["filename"]
	if name == "" {
		_, params, _ = header.ContentType()
		name = params["name"]
	}

	decoder := mime.WordDecoder{CharsetReader: charset.Reader}
	if decoded, err := decoder.DecodeHeader(name); err == nil {
		return decoded
	}

	return name
}
