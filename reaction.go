package courier

import (
	"context"
	"time"
)

// Reaction is the small piece of feedback a user attaches to a message
// already delivered: an emoji, most often a thumb or a heart.
//
// A reaction is deliberately NOT a Message. It carries no content to read,
// no parts, and answering it makes no sense — an application that received
// one through the message channel would treat it as something to reply to.
// It travels on its own channel instead, see ReactionProvider.
type Reaction struct {
	// MessageID is the message being reacted to. It lives in the same
	// namespace as the identifiers reported by incoming messages and
	// returned by Send, which is what lets an application recognise a
	// reaction to something it sent itself.
	MessageID MessageID

	// Channel is where the reacted message lives.
	Channel Channel

	// From is the user who reacted, never the author of the reacted
	// message.
	From User

	// Emoji is the reaction itself, for instance "👍".
	//
	// An EMPTY emoji means the user REMOVED their reaction. Platforms
	// report a removal as a reaction with no emoji rather than as a
	// distinct event, and those that do use a distinct event are
	// normalised to this shape.
	Emoji string

	// ReactedAt is when the user reacted, as reported by the platform.
	ReactedAt time.Time
}

// IsRemoval reports whether the reaction takes back a previous one.
func (r Reaction) IsRemoval() bool {
	return r.Emoji == ""
}

// ReactionProvider is implemented by providers able to report the reactions
// users leave on messages. Use the ListenReactions helper rather than
// asserting directly.
//
// It is a channel of its own rather than an extension of Listen because a
// reaction is not a message: every existing consumer selects on the message
// channel and would have to learn to ignore something it cannot answer.
type ReactionProvider interface {
	Provider

	// ListenReactions streams the reactions left on messages of the
	// channels the provider watches, including reactions on messages the
	// provider sent itself. Closing the context ends the stream.
	ListenReactions(ctx context.Context) (chan Reaction, error)
}

// ListenReactions streams reactions from provider, or returns a nil channel
// when it does not support them.
//
// A nil channel never fires in a select, so a consumer can wire the result
// unconditionally and let a provider without reactions simply stay silent:
//
//	reactions, err := courier.ListenReactions(ctx, provider)
//	...
//	select {
//	case msg := <-messages:
//	case reaction := <-reactions:
//	}
func ListenReactions(ctx context.Context, provider Provider) (chan Reaction, error) {
	reactive, ok := provider.(ReactionProvider)
	if !ok {
		return nil, nil
	}

	return reactive.ListenReactions(ctx)
}
