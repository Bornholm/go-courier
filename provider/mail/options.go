package mail

import (
	"crypto/tls"
	"time"

	"github.com/bornholm/go-courier"
)

type Options struct {
	SMTP SMTP
	IMAP IMAP
	// MaxInMemorySize is the size above which attachment content is spilled
	// to a temporary file rather than kept in memory.
	MaxInMemorySize int64
}

type OptionFunc func(opts *Options)

func WithIMAP(address string, username, password string) OptionFunc {
	return func(opts *Options) {
		opts.IMAP.Address = address
		opts.IMAP.Username = username
		opts.IMAP.Password = password
	}
}

func WithIMAPCheckInterval(interval time.Duration) OptionFunc {
	return func(opts *Options) {
		opts.IMAP.CheckInterval = interval
	}
}

// WithIMAPReadOnly makes the IMAP side strictly read-only, as a shared mailbox
// requires: folders are opened with EXAMINE, message bodies are fetched with
// BODY.PEEK (no \Seen flag is set) and new messages are found by UID, whatever
// their flags, instead of by the \Seen flag. A message already read by someone
// else is therefore still received, and nothing in the mailbox changes.
//
// The last UID seen is kept in memory, per folder and UIDVALIDITY. On the first
// check after the provider starts (or after the server changes the folder's
// UIDVALIDITY), messages received during the last since are fetched again: the
// application must deduplicate them, by Message-ID for instance.
func WithIMAPReadOnly(since time.Duration) OptionFunc {
	return func(opts *Options) {
		opts.IMAP.ReadOnly = true
		opts.IMAP.ReadOnlySince = since
	}
}

// WithIMAPTLSConfig sets the TLS configuration of the IMAP connection. By
// default the server certificate is verified against the system roots; a test
// can pass its own RootCAs, and InsecureSkipVerify must be asked for explicitly.
func WithIMAPTLSConfig(config *tls.Config) OptionFunc {
	return func(opts *Options) {
		opts.IMAP.TLSConfig = config
	}
}

func WithIMAPFolders(folders ...string) OptionFunc {
	return func(opts *Options) {
		opts.IMAP.Folders = folders
	}
}

func WithSMTP(address string, issuer string, username, password string) OptionFunc {
	return func(opts *Options) {
		opts.SMTP.Address = address
		opts.SMTP.Username = username
		opts.SMTP.Password = password
		opts.SMTP.Issuer = issuer
	}
}

func WithMaxInMemorySize(size int64) OptionFunc {
	return func(opts *Options) {
		opts.MaxInMemorySize = size
	}
}

func NewOptions(funcs ...OptionFunc) *Options {
	opts := &Options{
		SMTP: SMTP{},
		IMAP: IMAP{
			CheckInterval: time.Minute,
			Folders:       []string{"INBOX"},
			ReadOnlySince: 24 * time.Hour,
		},
		MaxInMemorySize: courier.DefaultMaxInMemorySize,
	}
	for _, fn := range funcs {
		fn(opts)
	}
	return opts
}

type SMTP struct {
	Address  string
	Username string
	Password string
	Issuer   string
}

type IMAP struct {
	Address       string
	Username      string
	Password      string
	CheckInterval time.Duration

	Folders []string

	// ReadOnly : see WithIMAPReadOnly.
	ReadOnly      bool
	ReadOnlySince time.Duration

	// TLSConfig of the connection; nil verifies the server certificate against
	// the system roots.
	TLSConfig *tls.Config
}
