package signal

// Options configure the Signal provider.
type Options struct {
	// Address of the signal-cli daemon: "tcp://host:port",
	// "unix:///path/to/socket", or a bare "host:port" (TCP).
	Address string

	// Account is the E.164 number of the local Signal account
	// (e.g. "+33612345678"). Required when the daemon runs without a fixed
	// account (multi-account mode); harmless otherwise, and always used as
	// the identity reported by Self.
	Account string
}

type OptionFunc func(opts *Options)

func NewOptions(funcs ...OptionFunc) *Options {
	opts := &Options{
		Address: "tcp://127.0.0.1:7583",
	}
	for _, fn := range funcs {
		fn(opts)
	}
	return opts
}

// WithAddress sets the daemon address.
func WithAddress(address string) OptionFunc {
	return func(opts *Options) {
		opts.Address = address
	}
}

// WithAccount sets the local account number.
func WithAccount(account string) OptionFunc {
	return func(opts *Options) {
		opts.Account = account
	}
}
