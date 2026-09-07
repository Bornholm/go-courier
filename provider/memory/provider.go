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

	reactionMutex     sync.RWMutex
	reactionListeners []chan courier.Reaction
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
func (p *Provider) Send(ctx context.Context, message courier.Message) (courier.MessageID, error) {
	p.sentMutex.Lock()
	p.sent = append(p.sent, message)
	p.sentMutex.Unlock()

	if !p.opts.Loopback {
		return message.ID(), nil
	}

	if err := p.Deliver(ctx, message); err != nil {
		return "", errors.WithStack(err)
	}

	return message.ID(), nil
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

// ListenReactions implements courier.ReactionProvider. Like Listen, each
// call gets its own channel and every reaction is fanned out to all of them.
func (p *Provider) ListenReactions(ctx context.Context) (chan courier.Reaction, error) {
	p.mutex.RLock()
	closed := p.closed
	p.mutex.RUnlock()

	if closed {
		return nil, errors.WithStack(courier.ErrClosed)
	}

	reactions := make(chan courier.Reaction, p.opts.BufferSize)

	p.reactionMutex.Lock()
	p.reactionListeners = append(p.reactionListeners, reactions)
	p.reactionMutex.Unlock()

	go func() {
		<-ctx.Done()
		p.removeReactionListener(reactions)
	}()

	return reactions, nil
}

// React simulates a user reacting to a message. An empty emoji simulates a
// user taking their reaction back, as on a real platform.
func (p *Provider) React(ctx context.Context, reaction courier.Reaction) error {
	p.mutex.RLock()
	closed := p.closed
	p.mutex.RUnlock()

	if closed {
		return errors.WithStack(courier.ErrClosed)
	}

	p.reactionMutex.RLock()
	listeners := make([]chan courier.Reaction, len(p.reactionListeners))
	copy(listeners, p.reactionListeners)
	p.reactionMutex.RUnlock()

	for _, listener := range listeners {
		select {
		case listener <- reaction:
		case <-ctx.Done():
			return errors.WithStack(ctx.Err())
		}
	}

	return nil
}

func (p *Provider) removeReactionListener(target chan courier.Reaction) {
	p.reactionMutex.Lock()
	defer p.reactionMutex.Unlock()

	for idx, listener := range p.reactionListeners {
		if listener != target {
			continue
		}

		p.reactionListeners = append(p.reactionListeners[:idx], p.reactionListeners[idx+1:]...)

		return
	}
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

	p.reactionMutex.Lock()
	for _, listener := range p.reactionListeners {
		close(listener)
	}
	p.reactionListeners = nil
	p.reactionMutex.Unlock()

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
		courier.CapabilityReactions,
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
		opts:              NewOptions(funcs...),
		listeners:         []chan courier.Message{},
		sent:              []courier.Message{},
		reactionListeners: []chan courier.Reaction{},
	}
}

var (
	_ courier.Provider           = &Provider{}
	_ courier.SelfProvider       = &Provider{}
	_ courier.ChannelResolver    = &Provider{}
	_ courier.CapabilityProvider = &Provider{}
	_ courier.ReactionProvider   = &Provider{}
)
