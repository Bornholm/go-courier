package whatsapp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/bornholm/go-courier"
	"github.com/mdp/qrterminal"
	"github.com/pkg/errors"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow"
	waLog "go.mau.fi/whatsmeow/util/log"

	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
)

func init() {
	store.DeviceProps.Os = proto.String("go-courier")
}

type Provider struct {
	dbPath string

	initErr  error
	initOnce sync.Once
	client   *whatsmeow.Client
}

// SetPresence implements courier.Provider.
func (p *Provider) SetPresence(ctx context.Context, presence courier.Presence) error {
	client, err := p.getClient(ctx)
	if err != nil {
		return errors.WithStack(err)
	}

	switch presence {
	case courier.PresenceOnline:
		if err := client.SendPresence(types.PresenceAvailable); err != nil {
			return errors.WithStack(err)
		}
	case courier.PresenceOffline:
		if err := client.SendPresence(types.PresenceUnavailable); err != nil {
			return errors.WithStack(err)
		}
	}

	return nil
}

// SetStatus implements courier.Provider.
func (p *Provider) SetStatus(ctx context.Context, status courier.Status, channelID string) error {
	client, err := p.getClient(ctx)
	if err != nil {
		return errors.WithStack(err)
	}

	jid, err := types.ParseJID(string(channelID))
	if err != nil {
		return errors.WithStack(err)
	}

	switch status {
	case courier.StatusTyping:
		if err := client.SendChatPresence(jid, types.ChatPresenceComposing, ""); err != nil {
			return errors.WithStack(err)
		}
	case courier.StatusIdle:
		if err := client.SendChatPresence(jid, types.ChatPresencePaused, ""); err != nil {
			return errors.WithStack(err)
		}
	}

	return nil
}

// Listen implements courier.Provider.
func (p *Provider) Listen(ctx context.Context) (chan courier.Message, error) {
	client, err := p.getClient(ctx)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if err := client.SendPresence(types.PresenceAvailable); err != nil {
		return nil, errors.WithStack(err)
	}

	messages := make(chan courier.Message)

	client.AddEventHandler(func(evt any) {
		slog.DebugContext(ctx, "received whatsapp event", slog.Any("event", evt))

		switch v := evt.(type) {
		case *events.Message:
			if v.Info.MessageSource.IsFromMe || v.Info.Type != "text" {
				return
			}

			if v.Info.MessageSource.IsGroup {
				jid := client.Store.ID
				if v.Message.ExtendedTextMessage != nil && jid != nil {
					mentions := v.Message.ExtendedTextMessage.GetContextInfo().GetMentionedJID()
					if !slices.Contains(mentions, jid.ToNonAD().String()) {
						return
					}
				} else {
					return
				}
			}

			var text string
			switch {
			case v.Message.ExtendedTextMessage != nil:
				text = *v.Message.ExtendedTextMessage.Text
			case v.Message.Conversation != nil:
				text = *v.Message.Conversation
			}

			if text == "" {
				return
			}

			message := courier.NewMessage(
				courier.MessageID(v.Info.ID),
				courier.ChannelID(v.Info.MessageSource.Chat.String()),
				courier.NewUser(courier.UserID(v.Info.MessageSource.Sender.String()), v.Info.PushName),
				courier.WithMessageMainPart(text),
			)

			messages <- message
		}
	})

	return messages, nil
}

// Send implements courier.Provider.
func (p *Provider) Send(ctx context.Context, message courier.Message) error {
	client, err := p.getClient(ctx)
	if err != nil {
		return errors.WithStack(err)
	}

	mainContent, err := courier.GetMessageMainContent(message)
	if err != nil {
		return errors.WithStack(err)
	}

	slog.DebugContext(ctx, "sending message", slog.Any("channelID", message.ChannelID), slog.String("message", mainContent))

	to, err := types.ParseJID(string(message.ChannelID()))
	if err != nil {
		return errors.WithStack(err)
	}

	_, err = client.SendMessage(ctx, to, &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			ContextInfo: &waE2E.ContextInfo{
				Expiration: proto.Uint32(uint32((24 * time.Hour).Seconds())),
			},
			Text: proto.String(mainContent),
		},
	})

	return errors.WithStack(err)
}

func (p *Provider) getClient(ctx context.Context) (*whatsmeow.Client, error) {
	p.initOnce.Do(func() {
		slog.DebugContext(ctx, "initializing whatsapp client")

		dbLog := waLog.Stdout("Database", "DEBUG", true)
		container, err := sqlstore.New(ctx, "sqlite3", fmt.Sprintf("%s?_foreign_keys=on", p.dbPath), dbLog)
		if err != nil {
			p.initErr = errors.WithStack(err)
			return
		}

		device, err := container.GetFirstDevice(ctx)
		if err != nil {
			p.initErr = errors.WithStack(err)
			return
		}

		clientLog := waLog.Stdout("Client", "DEBUG", true)

		client := whatsmeow.NewClient(device, clientLog)

		client.Store.PushName = "-"

		if client.Store.ID == nil {
			qrChan, err := client.GetQRChannel(ctx)
			if err != nil {
				p.initErr = errors.WithStack(err)
				return
			}

			if err := client.Connect(); err != nil {
				p.initErr = errors.WithStack(err)
				return
			}

			for evt := range qrChan {
				if evt.Event == "code" {
					qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
				} else {
					slog.DebugContext(ctx, "whatsapp client logged in")
				}
			}
		} else {
			if err := client.Connect(); err != nil {
				p.initErr = errors.WithStack(err)
				return
			}
		}

		p.client = client
	})
	if p.initErr != nil {
		return nil, errors.WithStack(p.initErr)
	}

	return p.client, nil
}

func NewProvider(dbPath string) *Provider {
	return &Provider{
		dbPath: dbPath,
	}
}

var _ courier.Provider = &Provider{}
