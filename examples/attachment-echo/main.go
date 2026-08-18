// Command attachment-echo shows what go-courier reports about an incoming
// message: the kind of channel it came from, who was mentioned, and every
// attachment it carries. Each attachment is echoed back, which exercises both
// the download and the upload path of a provider.
//
// Pick the provider with the PROVIDER environment variable:
//
//	PROVIDER=rest go run ./examples/attachment-echo
//	PROVIDER=whatsapp WHATSAPP_DB=whatsapp.db go run ./examples/attachment-echo
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/provider/rest"
	"github.com/bornholm/go-courier/provider/whatsapp"
	"github.com/pkg/errors"
)

func main() {
	slog.SetLogLoggerLevel(slog.LevelInfo)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	provider, err := newProvider()
	if err != nil {
		slog.ErrorContext(ctx, "could not create provider", slog.Any("error", err))
		os.Exit(1)
	}

	messages, err := provider.Listen(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "could not listen", slog.Any("error", errors.WithStack(err)))
		os.Exit(1)
	}

	self := resolveSelf(ctx, provider)

	slog.InfoContext(ctx, "listening, Ctrl+C to exit",
		slog.String("self", string(self.ID())),
		slog.Any("capabilities", capabilityNames(provider)),
	)

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

func newProvider() (courier.Provider, error) {
	switch name := os.Getenv("PROVIDER"); name {
	case "", "rest":
		address := os.Getenv("ADDRESS")
		if address == "" {
			address = ":8080"
		}

		return rest.NewProvider(
			rest.WithAddress(address),
			rest.WithTokens(map[string]courier.User{
				"demo-token": courier.NewUser("demo-user", "Demo User"),
			}),
		), nil

	case "whatsapp":
		dbPath := os.Getenv("WHATSAPP_DB")
		if dbPath == "" {
			dbPath = "whatsapp.db"
		}

		return whatsapp.NewProvider(whatsapp.WithDBPath(dbPath)), nil

	default:
		return nil, errors.Errorf("unknown provider %q", name)
	}
}

func handle(ctx context.Context, provider courier.Provider, self courier.User, message courier.Message) error {
	channel := message.Channel()

	content, err := courier.GetMessageMainContent(ctx, message)
	if err != nil && !errors.Is(err, courier.ErrNotFound) {
		return errors.WithStack(err)
	}

	slog.InfoContext(ctx, "received message",
		slog.String("id", string(message.ID())),
		slog.String("channel", string(channel.ChannelID())),
		slog.String("channelKind", string(channel.Kind())),
		slog.Bool("isGroup", courier.IsGroupChannel(channel)),
		slog.String("from", message.From().DisplayName()),
		slog.Bool("mentionsMe", courier.IsMentioned(message, self.ID())),
		slog.Any("mentions", mentionNames(message)),
		slog.String("content", content),
	)

	if parent, ok := courier.InReplyTo(message); ok {
		slog.InfoContext(ctx, "message replies to another one", slog.String("inReplyTo", string(parent)))
	}

	funcs := []courier.BaseMessageOptionFunc{
		courier.WithMessageInReplyTo(message.ID()),
	}

	summary := []string{fmt.Sprintf("channel kind: %s", channel.Kind())}

	for _, attachment := range courier.Attachments(message) {
		kind := courier.MediaKindOf(attachment.ContentType())

		// Reading the attachment is what actually downloads it: up to this
		// point only its metadata had crossed the network.
		content, err := courier.ReadPart(ctx, attachment)
		if err != nil {
			return errors.Wrapf(err, "could not read attachment %q", attachment.Name())
		}

		slog.InfoContext(ctx, "received attachment",
			slog.String("name", attachment.Name()),
			slog.String("filename", courier.FilenameFor(attachment)),
			slog.String("contentType", attachment.ContentType()),
			slog.String("kind", string(kind)),
			slog.Int("downloadedBytes", len(content)),
			slog.Bool("voiceNote", courier.IsVoiceNote(attachment)),
			slog.String("caption", attachment.Caption()),
		)

		summary = append(summary, fmt.Sprintf("%s (%s, %s, %d bytes)",
			courier.FilenameFor(attachment), attachment.ContentType(), kind, len(content)))

		// Only echo the file back if the provider can send attachments.
		if courier.HasCapability(provider, courier.CapabilitySendAttachments) {
			funcs = append(funcs, courier.WithMessagePart(attachment))
		}
	}

	funcs = append(funcs, courier.WithMessageMainPart(strings.Join(summary, "\n")))

	reply := courier.NewMessage(courier.RandomMessageID(), channel, self, funcs...)

	if err := provider.Send(ctx, reply); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// resolveSelf asks the provider who it is authenticated as, falling back on a
// placeholder for providers that cannot tell.
func resolveSelf(ctx context.Context, provider courier.Provider) courier.User {
	identified, ok := provider.(courier.SelfProvider)
	if !ok {
		return courier.NewUser("echo", "Echo")
	}

	self, err := identified.Self(ctx)
	if err != nil {
		slog.WarnContext(ctx, "could not resolve self", slog.Any("error", errors.WithStack(err)))
		return courier.NewUser("echo", "Echo")
	}

	return self
}

func capabilityNames(provider courier.Provider) []string {
	capable, ok := provider.(courier.CapabilityProvider)
	if !ok {
		return nil
	}

	names := make([]string, 0)

	for _, capability := range capable.Capabilities() {
		names = append(names, string(capability))
	}

	return names
}

func mentionNames(message courier.Message) []string {
	names := make([]string, 0)

	for _, mention := range courier.Mentions(message) {
		names = append(names, string(mention.UserID))
	}

	return names
}
