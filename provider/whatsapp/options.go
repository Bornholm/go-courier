package whatsapp

import (
	"context"

	"github.com/bornholm/go-courier"
)

// QRHandler receives the pairing events of a device that is not linked yet.
// code holds the payload to render as a QR code; it is refreshed every few
// seconds until the phone scans it. An empty code signals the end of the
// pairing sequence: linked reports whether it succeeded.
//
// Handlers run on the connection goroutine: keep them short, and never
// block on user input.
type QRHandler func(ctx context.Context, code string, linked bool)

type Options struct {
	// DBPath is the SQLite file holding the WhatsApp session.
	DBPath string
	// PushName is the display name advertised to contacts.
	PushName string
	// MaxInMemorySize is the size above which downloaded media is spilled to
	// a temporary file rather than kept in memory.
	MaxInMemorySize int64
	// QRHandler observes the pairing codes of an unlinked device. When nil,
	// the codes are printed to standard output as half-block QR codes — the
	// historical behaviour, which suits a terminal but not a web UI.
	QRHandler QRHandler
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

// WithQRHandler routes the pairing codes of an unlinked device to handler
// instead of printing them to standard output. This is what lets a web
// interface display the QR code and follow the pairing through.
func WithQRHandler(handler QRHandler) OptionFunc {
	return func(opts *Options) {
		opts.QRHandler = handler
	}
}
