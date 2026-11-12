package telegram

import (
	"net/http"
	"time"

	"github.com/bornholm/go-courier"
)

type Options struct {
	// Token is the bot token handed by @BotFather, "123456:ABC-DEF…". It is
	// part of every request URL: the provider never logs those URLs, and
	// strips them from the errors it returns.
	Token string
	// BaseURL is the Bot API root. Tests point it at a fake server; a
	// self-hosted Bot API server works too.
	BaseURL string
	// HTTPClient performs the Bot API calls. Its timeout must exceed
	// PollTimeout, or every long poll is cut short.
	HTTPClient *http.Client
	// PollTimeout is how long a getUpdates call waits for an update before
	// returning empty-handed.
	PollTimeout time.Duration
	// MaxInMemorySize is the size above which downloaded files are spilled
	// to a temporary file rather than kept in memory.
	MaxInMemorySize int64
	// MaxRetryDelay bounds the delay between two attempts when polling
	// fails.
	MaxRetryDelay time.Duration
}

type OptionFunc func(opts *Options)

func NewOptions(funcs ...OptionFunc) *Options {
	opts := &Options{
		BaseURL:         "https://api.telegram.org",
		HTTPClient:      &http.Client{Timeout: 2 * time.Minute},
		PollTimeout:     50 * time.Second,
		MaxInMemorySize: courier.DefaultMaxInMemorySize,
		MaxRetryDelay:   time.Minute,
	}
	for _, fn := range funcs {
		fn(opts)
	}
	return opts
}

func WithToken(token string) OptionFunc {
	return func(opts *Options) {
		opts.Token = token
	}
}

func WithBaseURL(baseURL string) OptionFunc {
	return func(opts *Options) {
		opts.BaseURL = baseURL
	}
}

func WithHTTPClient(client *http.Client) OptionFunc {
	return func(opts *Options) {
		opts.HTTPClient = client
	}
}

func WithPollTimeout(timeout time.Duration) OptionFunc {
	return func(opts *Options) {
		opts.PollTimeout = timeout
	}
}

func WithMaxInMemorySize(size int64) OptionFunc {
	return func(opts *Options) {
		opts.MaxInMemorySize = size
	}
}

func WithMaxRetryDelay(delay time.Duration) OptionFunc {
	return func(opts *Options) {
		opts.MaxRetryDelay = delay
	}
}
