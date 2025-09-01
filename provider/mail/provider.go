package mail

import (
	"context"
	"time"

	"log/slog"

	"github.com/bornholm/go-courier"
	"github.com/pkg/errors"
)

type Provider struct {
	opts *Options
}

// Listen implements courier.Provider.
func (p *Provider) Listen(ctx context.Context) (chan courier.Message, error) {
	send := make(chan courier.Message)

	go func() {
		check := func() {
			if err := p.checkMailbox(ctx, send); err != nil {
				slog.ErrorContext(ctx, "could not check mailbox", slog.Any("error", errors.WithStack(err)))
			}
		}

		check()

		ticker := time.NewTicker(p.opts.IMAP.CheckInterval)
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

var _ courier.Provider = &Provider{}

func NewProvider(funcs ...OptionFunc) *Provider {
	opts := NewOptions(funcs...)
	return &Provider{
		opts: opts,
	}
}
