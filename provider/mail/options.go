package mail

import "time"

type Options struct {
	SMTP SMTP
	IMAP IMAP
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

func NewOptions(funcs ...OptionFunc) *Options {
	opts := &Options{
		SMTP: SMTP{},
		IMAP: IMAP{
			CheckInterval: time.Minute,
			Folders:       []string{"INBOX"},
		},
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
