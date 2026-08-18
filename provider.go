package courier

import (
	"context"
	"slices"
)

type Provider interface {
	Listen(ctx context.Context) (chan Message, error)
	Send(ctx context.Context, message Message) error
}

type Presence string

const (
	PresenceOnline  Presence = "online"
	PresenceOffline Presence = "offline"
)

type PresenceProvider interface {
	Provider
	SetPresence(ctx context.Context, presence Presence) error
}

type Status string

const (
	StatusTyping Status = "typing"
	StatusIdle   Status = "idle"
)

type StatusProvider interface {
	Provider
	SetStatus(ctx context.Context, status Status, channelID ChannelID) error
}

// SelfProvider is implemented by providers able to tell which user they are
// authenticated as. It is the counterpart of IsMentioned: without knowing who
// "we" are, an application cannot decide whether a group message is addressed
// to it.
type SelfProvider interface {
	Provider
	Self(ctx context.Context) (User, error)
}

// ChannelResolver is implemented by providers able to describe a channel
// outside of message reception, typically to find out whether an identifier
// designates a group before sending anything to it.
type ChannelResolver interface {
	Provider
	Channel(ctx context.Context, channelID ChannelID) (Channel, error)
}

// Capability describes an optional provider behaviour. Applications use it to
// adapt what they send rather than failing at runtime, for instance by
// transcribing an image into text when the target platform cannot display
// one.
type Capability string

const (
	// CapabilityReceiveAttachments is declared when incoming messages may
	// carry attachments.
	CapabilityReceiveAttachments Capability = "receive_attachments"
	// CapabilitySendAttachments is declared when outgoing messages may carry
	// attachments.
	CapabilitySendAttachments Capability = "send_attachments"
	// CapabilityChannelKind is declared when Channel().Kind() is meaningful,
	// that is never ChannelKindUnknown.
	CapabilityChannelKind Capability = "channel_kind"
	// CapabilityMentions is declared when incoming messages report mentions.
	CapabilityMentions Capability = "mentions"
	// CapabilityThreads is declared when messages report the message they
	// reply to.
	CapabilityThreads  Capability = "threads"
	CapabilityPresence Capability = "presence"
	CapabilityStatus   Capability = "status"
)

// CapabilityProvider is implemented by providers advertising what they
// support.
type CapabilityProvider interface {
	Provider
	Capabilities() []Capability
}

// HasCapability reports whether the provider advertises the given capability.
// Providers not implementing CapabilityProvider advertise nothing.
func HasCapability(provider Provider, capability Capability) bool {
	capable, ok := provider.(CapabilityProvider)
	if !ok {
		return false
	}

	return slices.Contains(capable.Capabilities(), capability)
}
