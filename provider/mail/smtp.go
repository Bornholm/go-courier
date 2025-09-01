package mail

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"strconv"

	"github.com/bornholm/go-courier"
	"github.com/pkg/errors"
	gomail "gopkg.in/gomail.v2"
)

func (p *Provider) sendMessage(ctx context.Context, message courier.Message) error {
	source, err := p.findEmailByMessageID(ctx, string(message.ChannelID()))
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

	if len(source.InReplyTo) > 0 {
		email.SetHeader("In-Reply-To", source.InReplyTo...)
	}

	if len(source.References) > 0 {
		email.SetHeader("References", source.References...)
	}

	mainContent, err := courier.GetMessageMainContent(message)
	if err != nil {
		return errors.WithStack(err)
	}

	email.SetBody("text/plain", string(mainContent))

	slog.DebugContext(ctx, "sending email", slog.String("from", p.opts.SMTP.Issuer), slog.Any("to", toRawAddresses(source.From)))

	if err := gomail.Send(client, email); err != nil {
		return errors.WithStack(err)
	}

	if err := p.copyToSentFolder(ctx, email); err != nil {
		return errors.WithStack(err)
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
