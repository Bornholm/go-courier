package courier

import (
	"context"
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
