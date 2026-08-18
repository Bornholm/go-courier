package discord

import (
	"net/http"
	"time"

	"github.com/bornholm/go-courier"
)

type Options struct {
	// Token is the bot authentication token, prefixed with "Bot ".
	Token string
	// HTTPClient downloads attachments from the Discord CDN.
	HTTPClient *http.Client
	// MaxInMemorySize is the size above which downloaded attachments are
	// spilled to a temporary file rather than kept in memory.
	MaxInMemorySize int64
}

type OptionFunc func(opts *Options)

func NewOptions(funcs ...OptionFunc) *Options {
	opts := &Options{
		HTTPClient:      &http.Client{Timeout: 30 * time.Second},
		MaxInMemorySize: courier.DefaultMaxInMemorySize,
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

func WithHTTPClient(client *http.Client) OptionFunc {
	return func(opts *Options) {
		opts.HTTPClient = client
	}
}

func WithMaxInMemorySize(size int64) OptionFunc {
	return func(opts *Options) {
		opts.MaxInMemorySize = size
	}
}
