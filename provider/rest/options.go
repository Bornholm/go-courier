package rest

import (
	"net/http"

	"github.com/bornholm/go-courier"
	"github.com/pkg/errors"
)

// Authenticator resolves the user behind a request. It returns
// ErrUnauthorized when the request carries no valid credentials.
type Authenticator func(r *http.Request) (courier.User, error)

// ChannelResolverFunc describes a channel from its identifier.
type ChannelResolverFunc func(channelID courier.ChannelID) courier.Channel

type Options struct {
	// Address the HTTP server listens on.
	Address string
	// Authenticate resolves the user behind a request.
	Authenticate Authenticator
	// ResolveChannel describes a channel from its identifier.
	ResolveChannel ChannelResolverFunc
	// MaxUploadSize is the maximum size in bytes of a single uploaded file.
	MaxUploadSize int64
	// MaxInMemorySize is the size above which uploaded files are spilled to a
	// temporary file rather than kept in memory.
	MaxInMemorySize int64
	// HistorySize is how many outgoing messages are kept per channel, to be
	// replayed to clients reconnecting with a Last-Event-ID header.
	HistorySize int
	// InlineTextLimit is the size in bytes below which textual parts are
	// inlined in the JSON payload rather than only exposed as a URL.
	InlineTextLimit int64
	// SubscriberBufferSize is the capacity of each subscriber queue. A
	// subscriber falling that far behind is disconnected.
	SubscriberBufferSize int
	// IncomingBufferSize is the capacity of the channel returned by Listen.
	IncomingBufferSize int
	// CORSOrigins is the list of allowed origins. Empty disables CORS
	// headers, "*" allows any origin.
	CORSOrigins []string
}

type OptionFunc func(opts *Options)

func NewOptions(funcs ...OptionFunc) *Options {
	opts := &Options{
		Address: ":8080",
		// Without an explicit authenticator, the API is closed rather than
		// open: exposing every channel to anyone by default would be a poor
		// trade.
		Authenticate: func(r *http.Request) (courier.User, error) {
			return nil, errors.WithStack(ErrUnauthorized)
		},
		ResolveChannel: func(channelID courier.ChannelID) courier.Channel {
			return courier.NewChannel(channelID, courier.ChannelKindDirect, string(channelID))
		},
		MaxUploadSize:        32 << 20, // 32MiB
		MaxInMemorySize:      courier.DefaultMaxInMemorySize,
		HistorySize:          100,
		InlineTextLimit:      64 << 10, // 64KiB
		SubscriberBufferSize: 32,
		IncomingBufferSize:   16,
		CORSOrigins:          nil,
	}
	for _, fn := range funcs {
		fn(opts)
	}
	return opts
}

func WithAddress(address string) OptionFunc {
	return func(opts *Options) {
		opts.Address = address
	}
}

// WithTokens maps bearer tokens to users. Requests must carry an
// "Authorization: Bearer <token>" header.
func WithTokens(tokens map[string]courier.User) OptionFunc {
	return WithAuthenticator(func(r *http.Request) (courier.User, error) {
		token, ok := bearerToken(r)
		if !ok {
			return nil, errors.WithStack(ErrUnauthorized)
		}

		user, exists := tokens[token]
		if !exists {
			return nil, errors.WithStack(ErrUnauthorized)
		}

		return user, nil
	})
}

// WithAuthenticator plugs an application specific authentication scheme.
func WithAuthenticator(authenticate Authenticator) OptionFunc {
	return func(opts *Options) {
		opts.Authenticate = authenticate
	}
}

// WithAnonymous accepts every request, attributing incoming messages to the
// given user. Meant for local development only.
func WithAnonymous(user courier.User) OptionFunc {
	return WithAuthenticator(func(r *http.Request) (courier.User, error) {
		return user, nil
	})
}

// WithChannelKind reports the same kind for every channel.
func WithChannelKind(kind courier.ChannelKind) OptionFunc {
	return WithChannelResolver(func(channelID courier.ChannelID) courier.Channel {
		return courier.NewChannel(channelID, kind, string(channelID))
	})
}

// WithChannelResolver lets the application describe each channel, for
// instance to mark some of them as groups.
func WithChannelResolver(resolve ChannelResolverFunc) OptionFunc {
	return func(opts *Options) {
		opts.ResolveChannel = resolve
	}
}

func WithMaxUploadSize(size int64) OptionFunc {
	return func(opts *Options) {
		opts.MaxUploadSize = size
	}
}

func WithMaxInMemorySize(size int64) OptionFunc {
	return func(opts *Options) {
		opts.MaxInMemorySize = size
	}
}

func WithHistorySize(size int) OptionFunc {
	return func(opts *Options) {
		opts.HistorySize = size
	}
}

func WithInlineTextLimit(limit int64) OptionFunc {
	return func(opts *Options) {
		opts.InlineTextLimit = limit
	}
}

func WithSubscriberBufferSize(size int) OptionFunc {
	return func(opts *Options) {
		opts.SubscriberBufferSize = size
	}
}

func WithIncomingBufferSize(size int) OptionFunc {
	return func(opts *Options) {
		opts.IncomingBufferSize = size
	}
}

// WithCORSOrigins allows the given origins. Pass "*" to allow any.
func WithCORSOrigins(origins ...string) OptionFunc {
	return func(opts *Options) {
		opts.CORSOrigins = origins
	}
}
