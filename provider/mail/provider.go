package mail

import (
	"context"
	"sync"
	"time"

	"log/slog"

	"github.com/bornholm/go-courier"
	"github.com/pkg/errors"
)

type Provider struct {
	opts *Options

	releaseMutex sync.Mutex
	release      []courier.CloseFunc
}

// Listen implements courier.Provider. Mailboxes are polled at the configured
// interval.
func (p *Provider) Listen(ctx context.Context) (chan courier.Message, error) {
	send := make(chan courier.Message)

	go func() {
		defer p.releaseAll()

		check := func() {
			if err := p.checkMailbox(ctx, send); err != nil && !errors.Is(err, context.Canceled) {
				slog.ErrorContext(ctx, "could not check mailbox", slog.Any("error", errors.WithStack(err)))
			}
		}

		check()

		ticker := time.NewTicker(p.opts.IMAP.CheckInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				check()

			case <-ctx.Done():
				return
			}
		}
	}()

	return send, nil
}

// Send implements courier.Provider.
func (p *Provider) Send(ctx context.Context, message courier.Message) error {
	if err := p.sendMessage(ctx, message); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// Self implements courier.SelfProvider.
func (p *Provider) Self(ctx context.Context) (courier.User, error) {
	issuer := p.opts.SMTP.Issuer

	return courier.NewUser(courier.UserID(issuer), issuer), nil
}

// Capabilities implements courier.CapabilityProvider.
//
// Email has no notion of mentions, so CapabilityMentions is left out.
func (p *Provider) Capabilities() []courier.Capability {
	return []courier.Capability{
		courier.CapabilityReceiveAttachments,
		courier.CapabilitySendAttachments,
		courier.CapabilityChannelKind,
		courier.CapabilityThreads,
	}
}

func (p *Provider) trackRelease(release courier.CloseFunc) {
	p.releaseMutex.Lock()
	defer p.releaseMutex.Unlock()

	p.release = append(p.release, release)
}

func (p *Provider) releaseAll() {
	p.releaseMutex.Lock()
	release := p.release
	p.release = nil
	p.releaseMutex.Unlock()

	for _, fn := range release {
		if err := fn(); err != nil {
			slog.Error("could not release attachment content", slog.Any("error", errors.WithStack(err)))
		}
	}
}

func NewProvider(funcs ...OptionFunc) *Provider {
	return &Provider{
		opts:    NewOptions(funcs...),
		release: []courier.CloseFunc{},
	}
}

var (
	_ courier.Provider           = &Provider{}
	_ courier.SelfProvider       = &Provider{}
	_ courier.CapabilityProvider = &Provider{}
)
