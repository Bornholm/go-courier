package mail

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"slices"

	"log/slog"

	"github.com/DusanKasan/parsemail"
	"github.com/bornholm/go-courier"
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/pkg/errors"
	erp "github.com/web-ridge/email-reply-parser"
	"gopkg.in/gomail.v2"
)

func (p *Provider) checkMailbox(ctx context.Context, send chan courier.Message) error {
	for _, f := range p.opts.IMAP.Folders {
		slog.DebugContext(ctx, "checking mailbox", slog.String("folder", f))

		emails, err := p.fetchUnreadEmails(ctx, f)
		if err != nil {
			slog.ErrorContext(ctx, "could not fetch folder unread emails", slog.Any("error", errors.WithStack(err)))
			continue
		}

		for email := range emails {
			thread, err := p.fetchMessageThread(ctx, f, email)
			if err != nil {
				slog.ErrorContext(ctx, "could not fetch email thread", slog.Any("error", errors.WithStack(err)))
				continue
			}

			send <- thread[0]

			// if err := p.markAsUnseen(ctx, f, email.MessageID); err != nil {
			// 	slog.ErrorContext(ctx, "could not mark email as unseen", slog.Any("error", errors.WithStack(err)))
			// }
		}

		slog.DebugContext(ctx, "mailbox checked", slog.String("folder", f))
	}

	return nil
}

func (p *Provider) fetchMessageThread(ctx context.Context, mailbox string, email *parsemail.Email) ([]courier.Message, error) {
	mailboxes, err := p.fetchMailboxes()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if !slices.Contains(mailboxes, mailbox) {
		mailboxes = append([]string{mailbox}, mailboxes...)
	}

	messages := make([]courier.Message, 0, 1)

	for {
		alreadyFetched := slices.ContainsFunc(messages, func(m courier.Message) bool {
			return m.ID() == courier.MessageID(email.MessageID)
		})
		if alreadyFetched {
			break
		}

		reply := erp.Parse(email.TextBody)

		message := courier.NewMessage(
			courier.MessageID(email.MessageID),
			courier.ChannelID(email.MessageID),
			courier.NewUser(courier.UserID(email.From[0].Address), email.From[0].Name),
			courier.WithMessageSentAt(email.Date),
			courier.WithMessageMainPart(reply),
		)

		messages = append(messages, message)

		if len(email.InReplyTo) == 0 {
			break
		}

		for _, mb := range mailboxes {
			prev, err := p.fetchEmailByMessageID(ctx, mb, email.InReplyTo[0])
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					continue
				}

				return nil, errors.Wrapf(err, "could not fetch email '%s'", email.InReplyTo[0])
			}

			email = prev
			break
		}
	}

	return messages, nil
}

func (p *Provider) fetchMailboxes() ([]string, error) {
	client, err := p.getImapClient()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	defer client.Close()

	mailboxes, err := client.List("", "%", nil).Collect()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	names := make([]string, 0, len(mailboxes))

	for _, mb := range mailboxes {
		if slices.Contains(mb.Attrs, imap.MailboxAttrNoSelect) {
			continue
		}

		names = append(names, mb.Mailbox)
	}

	return names, nil
}

func (p *Provider) fetchEmailByMessageID(ctx context.Context, mailbox string, messageID string) (*parsemail.Email, error) {
	client, err := p.getImapClient()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	defer client.Close()

	slog.DebugContext(ctx, "fetching message", slog.String("messageID", messageID), slog.String("mailbox", mailbox))

	if _, err := client.Select(mailbox, nil).Wait(); err != nil {
		return nil, errors.WithStack(err)
	}

	data, err := client.UIDSearch(&imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{
			{
				Key:   "Message-Id",
				Value: messageID,
			},
		},
	}, nil).Wait()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	seqSet := new(imap.UIDSet)
	seqSet.AddNum(data.AllUIDs()...)

	fetchOptions := &imap.FetchOptions{
		UID:         true,
		Envelope:    true,
		BodySection: []*imap.FetchItemBodySection{{}},
	}

	fetchCmd := client.Fetch(*seqSet, fetchOptions)
	defer fetchCmd.Close()

	for {
		msg := fetchCmd.Next()
		if msg == nil {
			break
		}

		for {
			section := msg.Next()
			if section == nil {
				break
			}

			bodySection, ok := section.(imapclient.FetchItemDataBodySection)
			if !ok {
				continue
			}

			parsed, err := parsemail.Parse(bodySection.Literal)
			if err != nil {
				return nil, errors.WithStack(err)
			}

			return &parsed, nil
		}
	}

	return nil, errors.WithStack(ErrNotFound)
}

func (p *Provider) copyToSentFolder(ctx context.Context, email *gomail.Message) error {
	client, err := p.getImapClient()
	if err != nil {
		return errors.WithStack(err)
	}

	defer client.Close()

	mailboxes, err := client.List("", "%", nil).Collect()
	if err != nil {
		return errors.WithStack(err)
	}

	var sentMailbox string
	for _, mb := range mailboxes {
		if !slices.Contains(mb.Attrs, imap.MailboxAttrSent) {
			continue
		}

		sentMailbox = mb.Mailbox
		break
	}

	if sentMailbox == "" {
		return errors.WithStack(ErrNotFound)
	}

	if _, err := client.Select(sentMailbox, nil).Wait(); err != nil {
		return errors.WithStack(err)
	}

	var buff bytes.Buffer

	if _, err := email.WriteTo(&buff); err != nil {
		return errors.WithStack(err)
	}

	appendCmd := client.Append(sentMailbox, int64(buff.Len()), nil)

	if _, err := io.Copy(appendCmd, &buff); err != nil {
		return errors.WithStack(err)
	}

	if err := appendCmd.Close(); err != nil {
		return errors.WithStack(err)
	}

	if _, err := appendCmd.Wait(); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

func (p *Provider) fetchUnreadEmails(ctx context.Context, folder string) (chan *parsemail.Email, error) {
	client, err := p.getImapClient()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if _, err := client.Select(folder, nil).Wait(); err != nil {
		return nil, errors.WithStack(err)
	}

	data, err := client.UIDSearch(&imap.SearchCriteria{
		NotFlag: []imap.Flag{imap.FlagSeen},
	}, nil).Wait()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	emails := make(chan *parsemail.Email, 1)

	go func() {
		defer client.Close()
		defer close(emails)

		seqSet := new(imap.UIDSet)
		seqSet.AddNum(data.AllUIDs()...)

		fetchOptions := &imap.FetchOptions{
			UID:         true,
			Envelope:    true,
			BodySection: []*imap.FetchItemBodySection{{}},
		}

		fetchCmd := client.Fetch(*seqSet, fetchOptions)
		defer fetchCmd.Close()

		for {
			msg := fetchCmd.Next()
			if msg == nil {
				break
			}

			for {
				section := msg.Next()
				if section == nil {
					break
				}

				bodySection, ok := section.(imapclient.FetchItemDataBodySection)
				if !ok {
					continue
				}

				parsed, err := parsemail.Parse(bodySection.Literal)
				if err != nil {
					slog.ErrorContext(ctx, "could not parse email", slog.Any("error", errors.WithStack(err)))
					continue
				}

				emails <- &parsed

				break
			}
		}
	}()

	return emails, nil
}

func (p *Provider) getImapClient() (*imapclient.Client, error) {
	client, err := imapclient.DialTLS(p.opts.IMAP.Address, &imapclient.Options{
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	})
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if err := client.Login(p.opts.IMAP.Username, p.opts.IMAP.Password).Wait(); err != nil {
		return nil, errors.WithStack(err)
	}

	return client, nil
}

func (p *Provider) findEmailByMessageID(ctx context.Context, messageID string) (*parsemail.Email, error) {
	mailboxes, err := p.fetchMailboxes()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	for _, mb := range mailboxes {
		email, err := p.fetchEmailByMessageID(ctx, mb, messageID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}

			return nil, errors.WithStack(err)
		}

		return email, nil
	}

	return nil, errors.WithStack(ErrNotFound)
}
