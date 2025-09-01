package web

import (
	"context"
	"log/slog"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/provider/web/server"
	"github.com/pkg/errors"
)

type Provider struct {
	address string
	server  *server.Server
}

// Listen implements courier.Provider.
func (p *Provider) Listen(ctx context.Context) (chan courier.Message, error) {
	send := make(chan courier.Message)

	p.server = server.New(p.address, send)

	go func() {
		defer close(send)

		if err := p.server.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.ErrorContext(ctx, "could not run server", slog.Any("error", errors.WithStack(err)))
		}
	}()

	return send, nil
}

// Send implements courier.Provider.
func (p *Provider) Send(ctx context.Context, message courier.Message) error {
	if p.server == nil {
		return errors.New("server not initialized")
	}

	if err := p.server.Send(message); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

func NewProvider(address string) *Provider {
	return &Provider{
		address: address,
	}
}

var _ courier.Provider = &Provider{}
