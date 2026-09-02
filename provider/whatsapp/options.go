package whatsapp

import (
	"context"
	"time"

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
	// DisappearingTimer is the fallback lifetime marked on outgoing messages
	// in chats whose own setting is not known yet. Zero — the default —
	// sends plain, permanent messages: a bot must never turn a regular
	// conversation into a disappearing one behind the user's back.
	//
	// Once a message has been received from a chat, that chat's own setting
	// always wins over this value.
	DisappearingTimer time.Duration
	// LogLevel is the verbosity of whatsmeow's own logger: "DEBUG", "INFO",
	// "WARN" or "ERROR". Empty means "INFO".
	//
	// The default used to be DEBUG, which logs every protocol frame — a
	// keepalive pair every twenty-five seconds, forever. On a long-lived
	// deployment that buries every other line in the log, and it was found
	// the hard way: it made a real incident considerably harder to diagnose.
	// Debugging the protocol is a deliberate act, not a default.
	LogLevel string
}

type OptionFunc func(opts *Options)

func NewOptions(funcs ...OptionFunc) *Options {
	opts := &Options{
		DBPath:          "whatsapp.db",
		PushName:        "-",
		MaxInMemorySize: courier.DefaultMaxInMemorySize,
		LogLevel:        "INFO",
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

// WithLogLevel sets the verbosity of whatsmeow's own logger: "DEBUG",
// "INFO", "WARN" or "ERROR". Use DEBUG only to investigate the protocol —
// it prints every frame, keepalives included.
func WithLogLevel(level string) OptionFunc {
	return func(opts *Options) {
		opts.LogLevel = level
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

// WithDisappearingTimer sets the lifetime marked on outgoing messages sent to
// a chat whose own disappearing-messages setting has not been observed yet.
// The zero value, which is the default, marks no expiry at all.
func WithDisappearingTimer(timer time.Duration) OptionFunc {
	return func(opts *Options) {
		opts.DisappearingTimer = timer
	}
}
