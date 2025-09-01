package rocket

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"sync"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/syncx"
	"github.com/gopackage/ddp"
	"github.com/pkg/errors"
)

type Provider struct {
	serverURL *url.URL
	username  string
	password  string

	client      *ddp.Client
	clientMutex sync.Mutex
}

// SetStatus implements courier.Provider.
func (p *Provider) SetStatus(ctx context.Context, status courier.Status, channelID string) error {
	client, err := p.getClient()
	if err != nil {
		return errors.WithStack(err)
	}

	switch status {
	case courier.StatusTyping:
		_, err = client.Call("stream-notify-room",
			fmt.Sprintf("%s/typing", channelID),
			p.username,
			true,
		)
		if err != nil {
			return errors.WithStack(err)
		}
	case courier.StatusIdle:
		_, err = client.Call("stream-notify-room",
			fmt.Sprintf("%s/typing", channelID),
			p.username,
			false,
		)
		if err != nil {
			return errors.WithStack(err)
		}
	}

	return nil
}

// Status implements ddp.StatusListener.
func (p *Provider) Status(status int) {
	if status != ddp.DISCONNECTED {
		return
	}

	slog.Error("lost rocket.chat connection", slog.String("serverURL", p.serverURL.String()))

	p.clientMutex.Lock()
	defer p.clientMutex.Unlock()

	p.client.Close()
	p.client.Reconnect()
}

// Listen implements courier.Provider.
func (p *Provider) Listen(ctx context.Context) (chan courier.Message, error) {
	client, err := p.getClient()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	client.Sub("stream-room-messages", "__my_messages__", true)

	messageChan := make(chan courier.Message)

	roomSub := client.CollectionByName("stream-room-messages")
	roomSub.AddUpdateListener(&messageListener{
		username:         p.username,
		messageChan:      messageChan,
		receivedMessages: syncx.Map[string, struct{}]{},
	})

	return messageChan, nil
}

// Send implements courier.Provider.
func (p *Provider) Send(ctx context.Context, message courier.Message) error {
	client, err := p.getClient()
	if err != nil {
		return errors.WithStack(err)
	}

	data, err := courier.GetMessageMainContent(message)
	if err != nil {
		return errors.WithStack(err)
	}

	_, err = client.Call("sendMessage", map[string]string{
		"_id": string(message.ID()),
		"rid": string(message.ChannelID()),
		"msg": string(data),
	})
	if err != nil {
		return errors.WithStack(err)
	}

	return nil
}

func (p *Provider) getClient() (*ddp.Client, error) {
	p.clientMutex.Lock()
	defer p.clientMutex.Unlock()

	if p.client != nil {
		return p.client, nil
	}

	websocketURL := p.serverURL.JoinPath("/websocket")
	websocketURL.Scheme = "wss"

	slog.Info("connecting to rocket server", slog.String("serverURL", p.serverURL.String()))

	client := ddp.NewClient(websocketURL.String(), p.serverURL.String())

	if err := client.Connect(); err != nil {
		return nil, errors.WithStack(err)
	}

	client.AddStatusListener(p)

	_, err := client.Call("login", ddp.NewUsernameLogin(p.username, p.password))
	if err != nil {
		return nil, errors.WithStack(err)
	}

	slog.Info("connected and authenticated with rocket server", slog.String("serverURL", p.serverURL.String()))

	p.client = client

	return client, nil
}

func NewProvider(serverURL *url.URL, username, password string) *Provider {
	return &Provider{
		serverURL: serverURL,
		username:  username,
		password:  password,
	}
}

var _ courier.Provider = &Provider{}
var _ ddp.StatusListener = &Provider{}
