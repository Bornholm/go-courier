package whatsappcloud

import (
	"context"
	"net/http"
	"time"

	"github.com/bornholm/go-courier"
)

// Template is an approved WhatsApp message template, the only kind of
// message the platform accepts once the customer service window is closed.
type Template struct {
	// Name of the template, as approved in the WhatsApp Manager.
	Name string
	// Language code of the template translation, for instance "fr" or
	// "en_US".
	Language string
	// BodyParameters fill the {{1}}, {{2}}… placeholders of the template
	// body, in order. WhatsApp rejects parameters holding new lines, tabs
	// or more than four consecutive spaces.
	BodyParameters []string
}

// ReengageFunc chooses the template sent to a user whose customer service
// window is closed. preview is the beginning of the message that could not
// be delivered, flattened into a single line so that it can fill a template
// parameter.
//
// Returning false gives up: Send then fails with ErrWindowClosed.
type ReengageFunc func(ctx context.Context, recipient courier.ChannelID, preview string) (Template, bool)

type Options struct {
	// AccessToken authenticates calls to the Graph API: a system user token
	// with the whatsapp_business_messaging permission.
	AccessToken string
	// PhoneNumberID identifies the business phone number the provider
	// speaks as. It is NOT the phone number itself.
	PhoneNumberID string
	// AppSecret is the secret of the Meta application, used to check the
	// X-Hub-Signature-256 header of every webhook. Without it, the webhook
	// refuses everything.
	AppSecret string
	// VerifyToken is the token typed in the webhook configuration of the
	// Meta application, echoed back when Meta checks the endpoint.
	VerifyToken string
	// APIVersion is the Graph API version, for instance "v24.0".
	APIVersion string
	// BaseURL is the Graph API root. Tests point it at a fake server.
	BaseURL string
	// HTTPClient performs the Graph API calls.
	HTTPClient *http.Client
	// MaxInMemorySize is the size above which downloaded media is spilled to
	// a temporary file rather than kept in memory.
	MaxInMemorySize int64
	// MaxMediaSize bounds the size of a downloaded media. WhatsApp itself
	// caps documents at 100MB.
	MaxMediaSize int64
	// MaxWebhookSize bounds the size of a webhook request body.
	MaxWebhookSize int64
	// ServiceWindow is how long after a user's last message free-form
	// messages may still be sent to them. WhatsApp sets it to 24 hours.
	ServiceWindow time.Duration
	// Windows remembers when each user last wrote. The default keeps it in
	// memory, which forgets everything on restart: the provider then tries
	// free-form messages and falls back on a template when WhatsApp reports
	// the window closed.
	Windows WindowStore
	// Reengage chooses the template sent when the window is closed. Nil
	// means no template: such messages fail with ErrWindowClosed.
	Reengage ReengageFunc
	// OutboxTTL is how long messages held back by a closed window wait for
	// the user to write again.
	OutboxTTL time.Duration
	// OutboxSize bounds the number of messages held back per user; the
	// oldest are dropped first.
	OutboxSize int
}

type OptionFunc func(opts *Options)

func NewOptions(funcs ...OptionFunc) *Options {
	opts := &Options{
		APIVersion:      "v24.0",
		BaseURL:         "https://graph.facebook.com",
		HTTPClient:      &http.Client{Timeout: 2 * time.Minute},
		MaxInMemorySize: courier.DefaultMaxInMemorySize,
		MaxMediaSize:    100 << 20,
		MaxWebhookSize:  1 << 20,
		ServiceWindow:   24 * time.Hour,
		OutboxTTL:       7 * 24 * time.Hour,
		OutboxSize:      20,
	}
	for _, fn := range funcs {
		fn(opts)
	}
	if opts.Windows == nil {
		opts.Windows = NewMemoryWindowStore()
	}
	return opts
}

// WithCredentials sets the access token and the phone number the provider
// speaks as.
func WithCredentials(accessToken, phoneNumberID string) OptionFunc {
	return func(opts *Options) {
		opts.AccessToken = accessToken
		opts.PhoneNumberID = phoneNumberID
	}
}

// WithWebhookSecrets sets the application secret checking webhook
// signatures and the token answering Meta's verification request.
func WithWebhookSecrets(appSecret, verifyToken string) OptionFunc {
	return func(opts *Options) {
		opts.AppSecret = appSecret
		opts.VerifyToken = verifyToken
	}
}

func WithAPIVersion(version string) OptionFunc {
	return func(opts *Options) {
		opts.APIVersion = version
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

func WithMaxInMemorySize(size int64) OptionFunc {
	return func(opts *Options) {
		opts.MaxInMemorySize = size
	}
}

func WithMaxMediaSize(size int64) OptionFunc {
	return func(opts *Options) {
		opts.MaxMediaSize = size
	}
}

func WithMaxWebhookSize(size int64) OptionFunc {
	return func(opts *Options) {
		opts.MaxWebhookSize = size
	}
}

func WithServiceWindow(window time.Duration) OptionFunc {
	return func(opts *Options) {
		opts.ServiceWindow = window
	}
}

// WithWindowStore persists when each user last wrote, so that a restart does
// not forget which windows are still open.
func WithWindowStore(store WindowStore) OptionFunc {
	return func(opts *Options) {
		opts.Windows = store
	}
}

// WithReengage installs the choice of the template sent when the customer
// service window is closed.
func WithReengage(reengage ReengageFunc) OptionFunc {
	return func(opts *Options) {
		opts.Reengage = reengage
	}
}

func WithOutbox(ttl time.Duration, size int) OptionFunc {
	return func(opts *Options) {
		opts.OutboxTTL = ttl
		opts.OutboxSize = size
	}
}
