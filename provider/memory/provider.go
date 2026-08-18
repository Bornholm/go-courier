// Package memory provides an in-process courier.Provider, meant for tests and
// for developing applications without connecting to a real messaging
// platform.
package memory

import (
	"context"
	"sync"

	"github.com/bornholm/go-courier"
	"github.com/pkg/errors"
)

// DefaultChannelID is the channel used by NewChannel when none is given.
const DefaultChannelID courier.ChannelID = "memory"

type Provider struct {
	opts *Options

	mutex     sync.RWMutex
	listeners []chan courier.Message
	closed    bool

	sentMutex sync.RWMutex
	sent      []courier.Message
}

// Listen implements courier.Provider. Each call returns its own channel, and
// every delivered message is fanned out to all of them.
func (p *Provider) Listen(ctx context.Context) (chan courier.Message, error) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	if p.closed {
		return nil, errors.WithStack(courier.ErrClosed)
	}

	messages := make(chan courier.Message, p.opts.BufferSize)
	p.listeners = append(p.listeners, messages)

	go func() {
		<-ctx.Done()
		p.removeListener(messages)
	}()

	return messages, nil
}

// Send implements courier.Provider. Messages are recorded and, when loopback
// is enabled, delivered back to the listeners.
func (p *Provider) Send(ctx context.Context, message courier.Message) error {
	p.sentMutex.Lock()
	p.sent = append(p.sent, message)
	p.sentMutex.Unlock()

	if !p.opts.Loopback {
		return nil
	}

	if err := p.Deliver(ctx, message); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// Deliver simulates an incoming message, as if it came from the platform.
func (p *Provider) Deliver(ctx context.Context, message courier.Message) error {
	p.mutex.RLock()
	listeners := make([]chan courier.Message, len(p.listeners))
	copy(listeners, p.listeners)
	closed := p.closed
	p.mutex.RUnlock()

	if closed {
		return errors.WithStack(courier.ErrClosed)
	}

	for _, listener := range listeners {
		select {
		case listener <- message:
		case <-ctx.Done():
			return errors.WithStack(ctx.Err())
		}
	}

	return nil
}

// Sent returns the messages passed to Send, in order.
func (p *Provider) Sent() []courier.Message {
	p.sentMutex.RLock()
	defer p.sentMutex.RUnlock()

	sent := make([]courier.Message, len(p.sent))
	copy(sent, p.sent)

	return sent
}

// Reset clears the recorded messages.
func (p *Provider) Reset() {
	p.sentMutex.Lock()
	defer p.sentMutex.Unlock()

	p.sent = nil
}

// Close releases every listener channel.
func (p *Provider) Close() error {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	if p.closed {
		return nil
	}

	p.closed = true

	for _, listener := range p.listeners {
		close(listener)
	}

	p.listeners = nil

	return nil
}

// Self implements courier.SelfProvider.
func (p *Provider) Self(ctx context.Context) (courier.User, error) {
	return p.opts.Self, nil
}

// Channel implements courier.ChannelResolver.
func (p *Provider) Channel(ctx context.Context, channelID courier.ChannelID) (courier.Channel, error) {
	if channel, exists := p.opts.Channels[channelID]; exists {
		return channel, nil
	}

	return courier.NewChannel(channelID, p.opts.DefaultChannelKind, string(channelID)), nil
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

func (p *Provider) removeListener(target chan courier.Message) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	for idx, listener := range p.listeners {
		if listener != target {
			continue
		}

		p.listeners = append(p.listeners[:idx], p.listeners[idx+1:]...)
		close(listener)

		return
	}
}

func NewProvider(funcs ...OptionFunc) *Provider {
	return &Provider{
		opts:      NewOptions(funcs...),
		listeners: []chan courier.Message{},
		sent:      []courier.Message{},
	}
}

var (
	_ courier.Provider           = &Provider{}
	_ courier.SelfProvider       = &Provider{}
	_ courier.ChannelResolver    = &Provider{}
	_ courier.CapabilityProvider = &Provider{}
)
