package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/provider/rest"
	"github.com/pkg/errors"
)

// token is the bearer token clients must present. Hard coded here because
// this is a demo; a real application would read it from its configuration.
const token = "demo-token"

func main() {
	slog.SetLogLoggerLevel(slog.LevelDebug)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	address := os.Getenv("ADDRESS")
	if address == "" {
		address = ":8080"
	}

	provider := rest.NewProvider(
		rest.WithAddress(address),
		rest.WithTokens(map[string]courier.User{
			token: courier.NewUser("demo-user", "Demo User"),
		}),
		rest.WithCORSOrigins("*"),
	)

	messages, err := provider.Listen(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "could not listen", slog.Any("error", errors.WithStack(err)))
		os.Exit(1)
	}

	slog.InfoContext(ctx, "echo server listening", slog.String("address", address))
	slog.InfoContext(ctx, "stream events with: "+
		"curl -N -H 'Authorization: Bearer "+token+"' http://localhost"+address+"/channels/demo/events")
	slog.InfoContext(ctx, "send a message with: "+
		"curl -H 'Authorization: Bearer "+token+"' -F 'message={\"content\":\"hello\"}' "+
		"-F 'files=@note.ogg' http://localhost"+address+"/channels/demo/messages")

	for message := range messages {
		if err := echo(ctx, provider, message); err != nil {
			slog.ErrorContext(ctx, "could not echo message", slog.Any("error", errors.WithStack(err)))
		}
	}
}

// echo sends the message back, describing whatever came attached to it.
func echo(ctx context.Context, provider courier.Provider, message courier.Message) error {
	content, err := courier.GetMessageMainContent(ctx, message)
	if err != nil {
		return errors.WithStack(err)
	}

	slog.InfoContext(ctx, "received message",
		slog.String("channel", string(message.Channel().ChannelID())),
		slog.String("channelKind", string(message.Channel().Kind())),
		slog.String("from", string(message.From().ID())),
		slog.String("content", content),
	)

	funcs := []courier.BaseMessageOptionFunc{
		courier.WithMessageMainPart("You said: " + content),
		courier.WithMessageInReplyTo(message.ID()),
	}

	// Send every attachment back, so clients can check the round trip.
	for _, attachment := range courier.Attachments(message) {
		slog.InfoContext(ctx, "received attachment",
			slog.String("filename", courier.FilenameFor(attachment)),
			slog.String("contentType", attachment.ContentType()),
			slog.String("kind", string(courier.MediaKindOf(attachment.ContentType()))),
			slog.Int64("size", attachment.Size()),
			slog.Bool("voiceNote", courier.IsVoiceNote(attachment)),
		)

		funcs = append(funcs, courier.WithMessagePart(attachment))
	}

	reply := courier.NewMessage(
		courier.RandomMessageID(),
		message.Channel(),
		courier.NewUser("echo", "Echo"),
		funcs...,
	)

	if err := provider.Send(ctx, reply); err != nil {
		return errors.WithStack(err)
	}

	return nil
}
