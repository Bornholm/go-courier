package mail

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/mail"
	"strconv"

	"github.com/bornholm/go-courier"
	"github.com/pkg/errors"
	gomail "gopkg.in/gomail.v2"
)

func (p *Provider) sendMessage(ctx context.Context, message courier.Message) error {
	source, err := p.findSourceEmail(ctx, message)
	if err != nil {
		return errors.WithStack(err)
	}

	dialer, err := p.getSMTPDialer()
	if err != nil {
		return errors.WithStack(err)
	}

	client, err := dialer.Dial()
	if err != nil {
		return errors.WithStack(err)
	}

	defer client.Close()

	email := gomail.NewMessage()

	email.SetHeader("From", p.opts.SMTP.Issuer)
	email.SetHeader("To", toRawAddresses(source.From)...)
	email.SetHeader("Subject", fmt.Sprintf("Re: %s", source.Subject))

	if len(source.Cc) > 0 {
		email.SetHeader("Cc", toRawAddresses(source.Cc)...)
	}

	if len(source.Bcc) > 0 {
		email.SetHeader("Bcc", toRawAddresses(source.Bcc)...)
	}

	// Thread the reply onto the message it answers, so mail clients group the
	// conversation and the channel identifier stays stable.
	email.SetHeader("In-Reply-To", source.MessageID)
	email.SetHeader("References", append(source.References, source.MessageID)...)

	main, err := courier.GetMessageMainPart(message)
	if err != nil {
		return errors.WithStack(err)
	}

	content, err := courier.ReadPart(ctx, main)
	if err != nil {
		return errors.WithStack(err)
	}

	contentType := main.ContentType()
	if contentType == "" {
		contentType = courier.TextPlain
	}

	email.SetBody(contentType, string(content))

	if err := attachParts(ctx, email, message); err != nil {
		return errors.WithStack(err)
	}

	slog.DebugContext(ctx, "sending email",
		slog.String("from", p.opts.SMTP.Issuer),
		slog.Any("to", toRawAddresses(source.From)),
	)

	if err := gomail.Send(client, email); err != nil {
		return errors.WithStack(err)
	}

	if err := p.copyToSentFolder(ctx, email); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// attachParts joins every attachment of the message to the email. Inline
// parts are embedded so that an HTML body can reference them by name.
func attachParts(ctx context.Context, email *gomail.Message, message courier.Message) error {
	for _, attachment := range courier.Attachments(message) {
		// The HTML body is carried as an alternative, not as a file.
		if attachment.ContentType() == "text/html" && attachment.Disposition() == courier.DispositionInline {
			content, err := courier.ReadPart(ctx, attachment)
			if err != nil {
				return errors.WithStack(err)
			}

			email.AddAlternative("text/html", string(content))

			continue
		}

		copyContent := func(part courier.MessagePart) gomail.FileSetting {
			return gomail.SetCopyFunc(func(w io.Writer) error {
				reader, err := part.Reader(ctx)
				if err != nil {
					return errors.WithStack(err)
				}

				defer reader.Close()

				if _, err := io.Copy(w, reader); err != nil {
					return errors.WithStack(err)
				}

				return nil
			})
		}

		settings := []gomail.FileSetting{
			copyContent(attachment),
			gomail.SetHeader(map[string][]string{
				"Content-Type": {attachment.ContentType()},
			}),
		}

		if attachment.Disposition() == courier.DispositionInline {
			email.Embed(courier.FilenameFor(attachment), settings...)
			continue
		}

		email.Attach(courier.FilenameFor(attachment), settings...)
	}

	return nil
}

func (p *Provider) getSMTPDialer() (*gomail.Dialer, error) {
	host, rawPort, err := net.SplitHostPort(p.opts.SMTP.Address)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	port, err := strconv.ParseInt(rawPort, 10, 32)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	dialer := gomail.NewDialer(host, int(port), p.opts.SMTP.Username, p.opts.SMTP.Password)

	return dialer, nil
}

func toRawAddresses(addrs []*mail.Address) []string {
	raw := make([]string, len(addrs))
	for idx, addr := range addrs {
		raw[idx] = addr.Address
	}
	return raw
}
