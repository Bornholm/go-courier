package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/provider/whatsapp"
	"github.com/pkg/errors"
)

func main() {
	slog.SetLogLoggerLevel(slog.LevelInfo)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	dbPath := os.Getenv("WHATSAPP_DB")
	if dbPath == "" {
		dbPath = "whatsapp.db"
	}

	provider := whatsapp.NewProvider(
		whatsapp.WithDBPath(dbPath),
		whatsapp.WithPushName("GoCourier"),
	)

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
		slog.String("channelName", channel.Name()),
		slog.String("from", message.From().DisplayName()),
		slog.String("content", content),
	)

	for _, attachment := range courier.Attachments(message) {
		slog.InfoContext(ctx, "received attachment",
			slog.String("filename", courier.FilenameFor(attachment)),
			slog.String("contentType", attachment.ContentType()),
			slog.String("kind", string(courier.MediaKindOf(attachment.ContentType()))),
			slog.Int64("size", attachment.Size()),
			slog.Bool("voiceNote", courier.IsVoiceNote(attachment)),
		)
	}

	// In a group, only answer when explicitly addressed, otherwise the bot
	// would reply to every single message. This policy belongs to the
	// application, which is why the provider forwards everything.
	if courier.IsGroupChannel(channel) && !courier.IsMentioned(message, self.ID()) {
		slog.DebugContext(ctx, "ignoring group message without mention")
		return nil
	}

	reply := courier.NewMessage(
		courier.RandomMessageID(),
		channel,
		self,
		courier.WithMessageMainPart(fmt.Sprintf("You've just sent: '%s'", content)),
	)

	if err := provider.Send(ctx, reply); err != nil {
		return errors.WithStack(err)
	}

	return nil
}
