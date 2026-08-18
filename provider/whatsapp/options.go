package whatsapp

import (
	"github.com/bornholm/go-courier"
)

type Options struct {
	// DBPath is the SQLite file holding the WhatsApp session.
	DBPath string
	// PushName is the display name advertised to contacts.
	PushName string
	// MaxInMemorySize is the size above which downloaded media is spilled to
	// a temporary file rather than kept in memory.
	MaxInMemorySize int64
}

type OptionFunc func(opts *Options)

func NewOptions(funcs ...OptionFunc) *Options {
	opts := &Options{
		DBPath:          "whatsapp.db",
		PushName:        "-",
		MaxInMemorySize: courier.DefaultMaxInMemorySize,
	}
	for _, fn := range funcs {
		fn(opts)
	}
	return opts
}

func WithDBPath(path string) OptionFunc {
	return func(opts *Options) {
		opts.DBPath = path
	}
}

func WithPushName(name string) OptionFunc {
	return func(opts *Options) {
		opts.PushName = name
	}
}

func WithMaxInMemorySize(size int64) OptionFunc {
	return func(opts *Options) {
		opts.MaxInMemorySize = size
	}
}
