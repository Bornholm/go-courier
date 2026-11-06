package mail

import (
	"bytes"
	"context"
	"io"
	"slices"
	"time"

	"log/slog"

	"github.com/DusanKasan/parsemail"
	"github.com/bornholm/go-courier"
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/pkg/errors"
	"gopkg.in/gomail.v2"
)

func (p *Provider) checkMailbox(ctx context.Context, send chan courier.Message) error {
	for _, f := range p.opts.IMAP.Folders {
		slog.DebugContext(ctx, "checking mailbox", slog.String("folder", f))

		emails, commit, err := p.fetchNewEmails(ctx, f)
		if err != nil {
			slog.ErrorContext(ctx, "could not fetch folder new emails", slog.Any("error", errors.WithStack(err)))
			continue
		}

		for email := range emails {
			select {
			case send <- p.toMessage(email):
			case <-ctx.Done():
				return errors.WithStack(ctx.Err())
			}
		}

		// Read-only mode: the folder's last UID is remembered only once every
		// message was handed over; an interrupted check fetches them again.
		commit()

		slog.DebugContext(ctx, "mailbox checked", slog.String("folder", f))
	}

	return nil
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

	if _, err := client.Select(mailbox, p.selectOptions()).Wait(); err != nil {
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
		BodySection: []*imap.FetchItemBodySection{p.bodySection()},
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

// seenUID : last UID handed over in a folder, valid for its UIDVALIDITY.
type seenUID struct {
	validity uint32
	uid      imap.UID
}

// selectOptions : EXAMINE in read-only mode (the folder cannot be changed),
// SELECT otherwise.
func (p *Provider) selectOptions() *imap.SelectOptions {
	if p.opts.IMAP.ReadOnly {
		return &imap.SelectOptions{ReadOnly: true}
	}
	return nil
}

// bodySection : the whole message; BODY.PEEK in read-only mode, so that the
// \Seen flag is left as it is.
func (p *Provider) bodySection() *imap.FetchItemBodySection {
	return &imap.FetchItemBodySection{Peek: p.opts.IMAP.ReadOnly}
}

// fetchNewEmails returns the folder's new emails and a commit function to call
// once they were all handed over. By default, new means unread (and fetching
// them marks them read). In read-only mode (WithIMAPReadOnly), new means a UID
// above the last one handed over, or, on the first check or after a change of
// UIDVALIDITY, received during the last ReadOnlySince, whatever the flags.
func (p *Provider) fetchNewEmails(ctx context.Context, folder string) (chan *parsemail.Email, func(), error) {
	client, err := p.getImapClient()
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}

	selected, err := client.Select(folder, p.selectOptions()).Wait()
	if err != nil {
		client.Close()
		return nil, nil, errors.WithStack(err)
	}

	criteria := &imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}
	var last seenUID
	known := false
	if p.opts.IMAP.ReadOnly {
		p.seenMutex.Lock()
		last, known = p.seen[folder]
		p.seenMutex.Unlock()
		known = known && last.validity == selected.UIDValidity
		if known {
			uids := imap.UIDSet{}
			uids.AddRange(last.uid+1, 0) // last+1:*
			criteria = &imap.SearchCriteria{UID: []imap.UIDSet{uids}}
		} else {
			criteria = &imap.SearchCriteria{Since: time.Now().Add(-p.opts.IMAP.ReadOnlySince)}
		}
	}

	data, err := client.UIDSearch(criteria, nil).Wait()
	if err != nil {
		client.Close()
		return nil, nil, errors.WithStack(err)
	}

	uids := data.AllUIDs()
	if known {
		// « last+1:* » always matches the highest UID, even when it is not new.
		uids = slices.DeleteFunc(uids, func(uid imap.UID) bool { return uid <= last.uid })
	}

	commit := func() {}
	if p.opts.IMAP.ReadOnly {
		highest := last.uid
		if !known {
			highest = 0
		}
		for _, uid := range uids {
			highest = max(highest, uid)
		}
		if highest == 0 && !known && selected.UIDNext > 1 {
			// Nothing in the window: start from the folder's next UID (UIDNEXT is
			// optional in RFC 2060; without it, the window is searched again).
			highest = selected.UIDNext - 1
		}
		commit = func() {
			p.seenMutex.Lock()
			defer p.seenMutex.Unlock()
			if p.seen == nil {
				p.seen = map[string]seenUID{}
			}
			p.seen[folder] = seenUID{validity: selected.UIDValidity, uid: highest}
		}
	}

	emails := make(chan *parsemail.Email, 1)

	if len(uids) == 0 { // an empty FETCH is refused by some servers
		client.Close()
		close(emails)
		return emails, commit, nil
	}

	go func() {
		defer client.Close()
		defer close(emails)

		seqSet := new(imap.UIDSet)
		seqSet.AddNum(uids...)

		fetchOptions := &imap.FetchOptions{
			UID:         true,
			Envelope:    true,
			BodySection: []*imap.FetchItemBodySection{p.bodySection()},
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

	return emails, commit, nil
}

func (p *Provider) getImapClient() (*imapclient.Client, error) {
	// The server certificate is verified unless a TLS configuration says
	// otherwise (WithIMAPTLSConfig).
	client, err := imapclient.DialTLS(p.opts.IMAP.Address, &imapclient.Options{
		TLSConfig: p.opts.IMAP.TLSConfig,
	})
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if err := client.Login(p.opts.IMAP.Username, p.opts.IMAP.Password).Wait(); err != nil {
		return nil, errors.WithStack(err)
	}

	return client, nil
}

// findSourceEmail locates the email a reply answers. The message points at
// its parent through InReplyTo; failing that, the channel identifier is the
// root of the thread, which is still a valid target.
func (p *Provider) findSourceEmail(ctx context.Context, message courier.Message) (*parsemail.Email, error) {
	if parent, ok := courier.InReplyTo(message); ok {
		email, err := p.findEmailByMessageID(ctx, string(parent))
		if err == nil {
			return email, nil
		}

		if !errors.Is(err, ErrNotFound) {
			return nil, errors.WithStack(err)
		}

		slog.DebugContext(ctx, "could not find parent email, falling back on thread root",
			slog.String("messageID", string(parent)))
	}

	channel := message.Channel()
	if channel == nil {
		return nil, errors.WithStack(ErrNotFound)
	}

	email, err := p.findEmailByMessageID(ctx, string(channel.ChannelID()))
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return email, nil
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
