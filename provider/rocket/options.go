package rocket

import (
	"net/http"
	"net/url"
	"time"

	"github.com/bornholm/go-courier"
)

type Options struct {
	// ServerURL is the base URL of the Rocket.Chat server.
	ServerURL *url.URL
	Username  string
	Password  string
	// HTTPClient calls the REST API, used for file upload and download.
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

func WithServerURL(serverURL *url.URL) OptionFunc {
	return func(opts *Options) {
		opts.ServerURL = serverURL
	}
}

func WithCredentials(username, password string) OptionFunc {
	return func(opts *Options) {
		opts.Username = username
		opts.Password = password
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
