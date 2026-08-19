package rocket

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/syncx"
	"github.com/gopackage/ddp"
	"github.com/mitchellh/mapstructure"
	"github.com/pkg/errors"
)

type Provider struct {
	opts *Options

	rest *restClient

	client      *ddp.Client
	clientMutex sync.Mutex

	releaseMutex sync.Mutex
	release      []courier.CloseFunc
}

// SetStatus implements courier.StatusProvider.
//
// Two events are emitted for the same intent: recent clients listen on
// "user-activity", older ones on "typing". Sending both keeps the
// indicator visible whatever the server version, and neither event is
// worth failing on alone — a missing typing hint is cosmetic.
func (p *Provider) SetStatus(ctx context.Context, status courier.Status, channelID courier.ChannelID) error {
	client, err := p.getClient()
	if err != nil {
		return errors.WithStack(err)
	}

	typing := status == courier.StatusTyping

	activities := []string{}
	if typing {
		activities = append(activities, "user-typing")
	}

	_, activityErr := client.Call("stream-notify-room",
		fmt.Sprintf("%s/user-activity", channelID),
		p.opts.Username,
		activities,
		map[string]any{},
	)

	_, typingErr := client.Call("stream-notify-room",
		fmt.Sprintf("%s/typing", channelID),
		p.opts.Username,
		typing,
	)

	if activityErr != nil && typingErr != nil {
		return errors.WithStack(activityErr)
	}

	return nil
}

// Status implements ddp.StatusListener.
func (p *Provider) Status(status int) {
	if status != ddp.DISCONNECTED {
		return
	}

	slog.Error("lost rocket.chat connection", slog.String("serverURL", p.opts.ServerURL.String()))

	p.clientMutex.Lock()
	defer p.clientMutex.Unlock()

	p.client.Close()
	p.client.Reconnect()
}

// Listen implements courier.Provider.
//
// Every room the account takes part in is forwarded, private groups and
// public channels included. Filtering is the application's call: use
// Channel().Kind() and courier.IsMentioned.
func (p *Provider) Listen(ctx context.Context) (chan courier.Message, error) {
	client, err := p.getClient()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	client.Sub("stream-room-messages", "__my_messages__", true)

	messageChan := make(chan courier.Message)

	roomSub := client.CollectionByName("stream-room-messages")
	roomSub.AddUpdateListener(&messageListener{
		ctx:              ctx,
		provider:         p,
		username:         p.opts.Username,
		messageChan:      messageChan,
		receivedMessages: syncx.Map[string, struct{}]{},
	})

	go func() {
		<-ctx.Done()
		p.releaseAll()
	}()

	return messageChan, nil
}

// Send implements courier.Provider. Attachments go through the REST API,
// which is the only way to upload a file.
func (p *Provider) Send(ctx context.Context, message courier.Message) error {
	client, err := p.getClient()
	if err != nil {
		return errors.WithStack(err)
	}

	channel := message.Channel()
	if channel == nil {
		return errors.New("message has no channel")
	}

	content, err := courier.GetMessageMainContent(ctx, message)
	if err != nil && !errors.Is(err, courier.ErrNotFound) {
		return errors.WithStack(err)
	}

	attachments := courier.Attachments(message)

	for idx, attachment := range attachments {
		// Only the first upload carries the text, otherwise it would be
		// repeated under every file.
		description := ""
		if idx == 0 {
			description = content
		}

		if err := p.rest.upload(ctx, channel.ChannelID(), attachment, description); err != nil {
			return errors.WithStack(err)
		}
	}

	// The text has already been sent as the description of the first upload.
	if len(attachments) > 0 {
		return nil
	}

	if content == "" {
		return errors.New("message has neither content nor attachment")
	}

	payload := map[string]any{
		"_id": string(message.ID()),
		"rid": string(channel.ChannelID()),
		"msg": content,
	}

	if parent, ok := courier.InReplyTo(message); ok {
		payload["tmid"] = string(parent)
	}

	if _, err := client.Call("sendMessage", payload); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// Self implements courier.SelfProvider.
func (p *Provider) Self(ctx context.Context) (courier.User, error) {
	if _, err := p.getClient(); err != nil {
		return nil, errors.WithStack(err)
	}

	creds, err := p.rest.getCredentials()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return courier.NewUser(courier.UserID(creds.userID), p.opts.Username), nil
}

// Capabilities implements courier.CapabilityProvider.
func (p *Provider) Capabilities() []courier.Capability {
	return []courier.Capability{
		courier.CapabilityReceiveAttachments,
		courier.CapabilitySendAttachments,
		courier.CapabilityChannelKind,
		courier.CapabilityMentions,
		courier.CapabilityThreads,
		courier.CapabilityStatus,
	}
}

func (p *Provider) trackRelease(release courier.CloseFunc) {
	p.releaseMutex.Lock()
	defer p.releaseMutex.Unlock()

	p.release = append(p.release, release)
}

func (p *Provider) releaseAll() {
	p.releaseMutex.Lock()
	release := p.release
	p.release = nil
	p.releaseMutex.Unlock()

	for _, fn := range release {
		if err := fn(); err != nil {
			slog.Error("could not release attachment content", slog.Any("error", errors.WithStack(err)))
		}
	}
}

func (p *Provider) getClient() (*ddp.Client, error) {
	p.clientMutex.Lock()
	defer p.clientMutex.Unlock()

	if p.client != nil {
		return p.client, nil
	}

	websocketURL := p.opts.ServerURL.JoinPath("/websocket")
	websocketURL.Scheme = "wss"

	slog.Info("connecting to rocket server", slog.String("serverURL", p.opts.ServerURL.String()))

	client := ddp.NewClient(websocketURL.String(), p.opts.ServerURL.String())

	if err := client.Connect(); err != nil {
		return nil, errors.WithStack(err)
	}

	client.AddStatusListener(p)

	result, err := client.Call("login", ddp.NewUsernameLogin(p.opts.Username, p.opts.Password))
	if err != nil {
		return nil, errors.WithStack(err)
	}

	// The login result carries the credentials the REST API expects, which is
	// what makes file upload and download possible.
	if err := p.storeCredentials(result); err != nil {
		return nil, errors.WithStack(err)
	}

	slog.Info("connected and authenticated with rocket server", slog.String("serverURL", p.opts.ServerURL.String()))

	p.client = client

	return client, nil
}

// loginResult is the subset of the DDP login response the REST API needs.
type loginResult struct {
	ID    string `mapstructure:"id"`
	Token string `mapstructure:"token"`
}

func (p *Provider) storeCredentials(result any) error {
	login := loginResult{}

	if err := mapstructure.Decode(result, &login); err != nil {
		return errors.Wrap(err, "could not decode login result")
	}

	if login.ID == "" || login.Token == "" {
		return errors.New("login result carried no credentials")
	}

	p.rest.setCredentials(login.ID, login.Token)

	return nil
}

func NewProvider(funcs ...OptionFunc) *Provider {
	opts := NewOptions(funcs...)

	return &Provider{
		opts:    opts,
		rest:    newRESTClient(opts.ServerURL, opts.HTTPClient),
		release: []courier.CloseFunc{},
	}
}

var (
	_ courier.Provider           = &Provider{}
	_ courier.StatusProvider     = &Provider{}
	_ courier.SelfProvider       = &Provider{}
	_ courier.CapabilityProvider = &Provider{}
	_ ddp.StatusListener         = &Provider{}
)
