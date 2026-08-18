package memory

import (
	"github.com/bornholm/go-courier"
)

type Options struct {
	// BufferSize is the capacity of the channels returned by Listen.
	BufferSize int
	// Loopback delivers sent messages back to the listeners, turning the
	// provider into an echo server.
	Loopback bool
	// Self is the user the provider is authenticated as.
	Self courier.User
	// Channels holds the known channels, resolved by Channel.
	Channels map[courier.ChannelID]courier.Channel
	// DefaultChannelKind is reported for channels absent from Channels.
	DefaultChannelKind courier.ChannelKind
}

type OptionFunc func(opts *Options)

func NewOptions(funcs ...OptionFunc) *Options {
	opts := &Options{
		BufferSize:         16,
		Loopback:           false,
		Self:               courier.NewUser("memory", "Memory"),
		Channels:           map[courier.ChannelID]courier.Channel{},
		DefaultChannelKind: courier.ChannelKindDirect,
	}
	for _, fn := range funcs {
		fn(opts)
	}
	return opts
}

func WithBufferSize(size int) OptionFunc {
	return func(opts *Options) {
		opts.BufferSize = size
	}
}

// WithLoopback makes Send deliver messages back to the listeners.
func WithLoopback(loopback bool) OptionFunc {
	return func(opts *Options) {
		opts.Loopback = loopback
	}
}

func WithSelf(self courier.User) OptionFunc {
	return func(opts *Options) {
		opts.Self = self
	}
}

// WithChannels declares known channels, so that Channel reports their real
// kind and name.
func WithChannels(channels ...courier.Channel) OptionFunc {
	return func(opts *Options) {
		for _, channel := range channels {
			opts.Channels[channel.ChannelID()] = channel
		}
	}
}

func WithDefaultChannelKind(kind courier.ChannelKind) OptionFunc {
	return func(opts *Options) {
		opts.DefaultChannelKind = kind
	}
}
