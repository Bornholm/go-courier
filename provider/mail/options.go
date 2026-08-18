package mail

import (
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
}
