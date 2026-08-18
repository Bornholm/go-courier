package mail

import (
	"io"
	"strconv"

	"github.com/DusanKasan/parsemail"
	"github.com/bornholm/go-courier"
	erp "github.com/web-ridge/email-reply-parser"
)

// toMessage converts a parsed email.
//
// Attachment readers are backed by the IMAP fetch command and die with it, so
// their content is buffered right away rather than lazily.
func (p *Provider) toMessage(email *parsemail.Email) courier.Message {
	funcs := []courier.BaseMessageOptionFunc{
		courier.WithMessageSentAt(email.Date),
		// Quotations are stripped: the reply alone is what the sender wrote.
		courier.WithMessageMainPart(erp.Parse(email.TextBody)),
	}

	if email.HTMLBody != "" {
		funcs = append(funcs, courier.WithMessagePart(courier.NewAttachment(
			"", "text/html", courier.OpenerFromString(email.HTMLBody),
			courier.WithAttachmentName("body.html"),
			courier.WithAttachmentDisposition(courier.DispositionInline),
			courier.WithAttachmentSize(int64(len(email.HTMLBody))),
		)))
	}

	for idx, attachment := range email.Attachments {
		funcs = append(funcs, courier.WithMessagePart(p.toAttachment(
			attachment.Filename, attachment.ContentType, attachment.Data,
			partName("att", idx), courier.DispositionAttachment,
		)))
	}

	for idx, embedded := range email.EmbeddedFiles {
		// The CID is how the HTML body references the file, so it is the
		// natural part name.
		name := embedded.CID
		if name == "" {
			name = partName("embedded", idx)
		}

		funcs = append(funcs, courier.WithMessagePart(p.toAttachment(
			"", embedded.ContentType, embedded.Data,
			name, courier.DispositionInline,
		)))
	}

	if len(email.InReplyTo) > 0 {
		funcs = append(funcs, courier.WithMessageInReplyTo(courier.MessageID(email.InReplyTo[0])))
	}

	return courier.NewMessage(
		courier.MessageID(email.MessageID),
		courier.NewChannel(threadID(email), kindOf(email), email.Subject),
		senderOf(email),
		funcs...,
	)
}

func (p *Provider) toAttachment(filename, contentType string, data io.Reader, name string, disposition courier.Disposition) courier.Attachment {
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	open, release := courier.BufferedOpener(
		courier.OpenerOnce(io.NopCloser(data)),
		p.opts.MaxInMemorySize,
	)

	p.trackRelease(release)

	return courier.NewAttachment(filename, contentType, open,
		courier.WithAttachmentName(name),
		courier.WithAttachmentDisposition(disposition),
	)
}

// threadID identifies the conversation an email belongs to, so that every
// message of a thread shares one stable channel. The root message identifier
// is the natural key: References lists it first, and a message starting a
// thread is its own root.
func threadID(email *parsemail.Email) courier.ChannelID {
	if len(email.References) > 0 {
		return courier.ChannelID(email.References[0])
	}

	if len(email.InReplyTo) > 0 {
		return courier.ChannelID(email.InReplyTo[0])
	}

	return courier.ChannelID(email.MessageID)
}

// kindOf tells a one to one exchange from a wider one. Several recipients, or
// a mailing list header, mean the reply will be read by more than one person.
func kindOf(email *parsemail.Email) courier.ChannelKind {
	if email.Header.Get("List-Id") != "" {
		return courier.ChannelKindPublic
	}

	if len(email.To)+len(email.Cc) > 1 {
		return courier.ChannelKindGroup
	}

	return courier.ChannelKindDirect
}

// senderOf returns the sender, falling back on an empty user rather than
// panicking on a malformed email without a From header.
func senderOf(email *parsemail.Email) courier.User {
	if len(email.From) == 0 {
		return courier.NewUser("", "")
	}

	from := email.From[0]

	return courier.NewUser(courier.UserID(from.Address), from.Name)
}

func partName(prefix string, index int) string {
	return prefix + "-" + strconv.Itoa(index)
}
