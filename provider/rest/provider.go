// Package rest exposes a courier.Provider over a JSON HTTP API: clients post
// incoming messages as multipart requests and receive outgoing ones as server
// sent events.
//
//	POST /channels/{channelID}/messages          incoming message (multipart)
//	GET  /channels/{channelID}/events            outgoing messages (SSE)
//	GET  /channels/{channelID}                   channel metadata
//	GET  /messages/{messageID}/parts/{partName}  raw part content
//	GET  /healthz                                liveness probe
package rest

import (
	"context"
	"log/slog"
	"sync"

	"github.com/bornholm/go-courier"
	"github.com/pkg/errors"
)

type Provider struct {
	opts *Options

	mutex  sync.Mutex
	server *server
}

// Listen implements courier.Provider. It starts the HTTP server and returns
// the channel carrying the messages posted by clients.
func (p *Provider) Listen(ctx context.Context) (chan courier.Message, error) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	if p.server != nil {
		return nil, errors.New("provider is already listening")
	}

	// Buffered so that a POST is acknowledged as soon as the message is
	// queued, without waiting for the application to consume it. Once the
	// buffer is full, clients are held back rather than silently dropped.
	incoming := make(chan courier.Message, p.opts.IncomingBufferSize)

	server := newServer(p.opts, incoming)
	p.server = server

	go func() {
		defer close(incoming)

		if err := server.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.ErrorContext(ctx, "could not run server", slog.Any("error", errors.WithStack(err)))
		}

		p.mutex.Lock()
		p.server = nil
		p.mutex.Unlock()
	}()

	return incoming, nil
}

// Send implements courier.Provider. The message is handed to the subscribers
// of its channel; its parts stay downloadable for as long as the history
// keeps it.
func (p *Provider) Send(ctx context.Context, message courier.Message) error {
	p.mutex.Lock()
	server := p.server
	p.mutex.Unlock()

	if server == nil {
		return errors.New("provider is not listening")
	}

	server.publish(message)

	return nil
}

// Channel implements courier.ChannelResolver.
func (p *Provider) Channel(ctx context.Context, channelID courier.ChannelID) (courier.Channel, error) {
	return p.opts.ResolveChannel(channelID), nil
}

// Capabilities implements courier.CapabilityProvider.
func (p *Provider) Capabilities() []courier.Capability {
	return []courier.Capability{
		courier.CapabilityReceiveAttachments,
		courier.CapabilitySendAttachments,
		courier.CapabilityChannelKind,
		courier.CapabilityMentions,
		courier.CapabilityThreads,
	}
}

func NewProvider(funcs ...OptionFunc) *Provider {
	return &Provider{
		opts: NewOptions(funcs...),
	}
}

var (
	_ courier.Provider           = &Provider{}
	_ courier.ChannelResolver    = &Provider{}
	_ courier.CapabilityProvider = &Provider{}
)
