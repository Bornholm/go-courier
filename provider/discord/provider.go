package discord

import (
	"context"
	"log/slog"

	"github.com/bornholm/go-courier"
	"github.com/bwmarrin/discordgo"
	"github.com/pkg/errors"
)

type Provider struct {
	token   string
	session *discordgo.Session
}

// Listen implements courier.Provider.
func (p *Provider) Listen(ctx context.Context) (chan courier.Message, error) {
	session, err := discordgo.New(p.token)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	session.Identify.Intents = discordgo.MakeIntent(discordgo.IntentsAllWithoutPrivileged)

	messages := make(chan courier.Message)

	session.AddHandler(p.createMessageHandler(ctx, messages))

	if err := session.Open(); err != nil {
		return nil, errors.WithStack(err)
	}

	p.session = session

	go func() {
		<-ctx.Done()
		if p.session != nil {
			p.session.Close()
			p.session = nil
		}
	}()

	return messages, nil

}

func (p *Provider) createMessageHandler(ctx context.Context, messages chan courier.Message) func(s *discordgo.Session, m *discordgo.MessageCreate) {
	return func(s *discordgo.Session, m *discordgo.MessageCreate) {
		if m.Author.ID == s.State.User.ID {
			return
		}

		channel, err := s.UserChannelCreate(m.Author.ID)
		if err != nil {
			slog.ErrorContext(ctx, "could not create channel", slog.Any("error", errors.WithStack(err)))
			return
		}

		message := courier.NewMessage(
			courier.MessageID(m.ID),
			courier.ChannelID(channel.ID),
			courier.NewUser(courier.UserID(m.Author.ID), m.Author.Username),
			courier.WithMessageMainPart(m.Content),
		)

		messages <- message
	}
}

// Send implements courier.Provider.
func (p *Provider) Send(ctx context.Context, message courier.Message) error {
	mainContent, err := courier.GetMessageMainContent(message)
	if err != nil {
		return errors.WithStack(err)
	}

	_, err = p.session.ChannelMessageSend(string(message.ChannelID()), mainContent)
	if err != nil {
		return errors.WithStack(err)
	}

	return nil
}

func NewProvider(token string) *Provider {
	return &Provider{
		token: token,
	}
}

var _ courier.Provider = &Provider{}
