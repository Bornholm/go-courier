package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/provider/discord"
	"github.com/pkg/errors"
)

func main() {
	slog.SetLogLoggerLevel(slog.LevelInfo)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	botToken := os.Getenv("DISCORD_BOT_TOKEN")
	if botToken == "" {
		slog.ErrorContext(ctx, "DISCORD_BOT_TOKEN is not set")
		os.Exit(1)
	}

	provider := discord.NewProvider(discord.WithToken("Bot " + botToken))

	messages, err := provider.Listen(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "could not listen", slog.Any("error", errors.WithStack(err)))
		os.Exit(1)
	}

	self, err := provider.Self(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "could not resolve self", slog.Any("error", errors.WithStack(err)))
		os.Exit(1)
	}

	slog.InfoContext(ctx, "listening, Ctrl+C to exit", slog.String("self", string(self.ID())))

	for {
		select {
		case message, ok := <-messages:
			if !ok {
				return
			}

			if err := handle(ctx, provider, self, message); err != nil {
				slog.ErrorContext(ctx, "could not handle message", slog.Any("error", errors.WithStack(err)))
			}

		case <-ctx.Done():
			return
		}
	}
}

func handle(ctx context.Context, provider courier.Provider, self courier.User, message courier.Message) error {
	channel := message.Channel()

	content, err := courier.GetMessageMainContent(ctx, message)
	if err != nil {
		return errors.WithStack(err)
	}

	slog.InfoContext(ctx, "received message",
		slog.String("channel", string(channel.ChannelID())),
		slog.String("channelKind", string(channel.Kind())),
		slog.String("from", message.From().DisplayName()),
		slog.String("content", content),
		slog.Int("attachments", len(courier.Attachments(message))),
	)

	// Outside of a private conversation, only answer when addressed.
	if courier.IsGroupChannel(channel) && !courier.IsMentioned(message, self.ID()) {
		return nil
	}

	reply := courier.NewMessage(
		courier.RandomMessageID(),
		channel,
		self,
		courier.WithMessageMainPart(fmt.Sprintf("You've just sent: '%s'", content)),
		courier.WithMessageInReplyTo(message.ID()),
	)

	if err := provider.Send(ctx, reply); err != nil {
		return errors.WithStack(err)
	}

	return nil
}
