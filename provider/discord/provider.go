package discord

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/bornholm/go-courier"
	"github.com/bwmarrin/discordgo"
	"github.com/pkg/errors"
)

type Provider struct {
	opts *Options

	mutex   sync.Mutex
	session *discordgo.Session

	releaseMutex sync.Mutex
	release      []courier.CloseFunc
}

// Listen implements courier.Provider.
//
// Every message the bot can see is forwarded, guild channels and attachments
// included. Filtering is the application's call: use Channel().Kind() and
// courier.IsMentioned.
func (p *Provider) Listen(ctx context.Context) (chan courier.Message, error) {
	session, err := discordgo.New(p.opts.Token)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	session.Identify.Intents = discordgo.MakeIntent(discordgo.IntentsAllWithoutPrivileged)

	messages := make(chan courier.Message)

	session.AddHandler(p.createMessageHandler(ctx, messages))

	if err := session.Open(); err != nil {
		return nil, errors.WithStack(err)
	}

	p.mutex.Lock()
	p.session = session
	p.mutex.Unlock()

	go func() {
		<-ctx.Done()

		p.mutex.Lock()
		defer p.mutex.Unlock()

		if p.session != nil {
			p.session.Close()
			p.session = nil
		}

		p.releaseAll()
	}()

	return messages, nil
}

func (p *Provider) createMessageHandler(ctx context.Context, messages chan courier.Message) func(s *discordgo.Session, m *discordgo.MessageCreate) {
	return func(s *discordgo.Session, m *discordgo.MessageCreate) {
		if s.State.User != nil && m.Author.ID == s.State.User.ID {
			return
		}

		message := p.toMessage(ctx, s, m)
		if message == nil {
			return
		}

		select {
		case messages <- message:
		case <-ctx.Done():
		}
	}
}

func (p *Provider) toMessage(ctx context.Context, session *discordgo.Session, m *discordgo.MessageCreate) courier.Message {
	if m.Content == "" && len(m.Attachments) == 0 {
		return nil
	}

	funcs := []courier.BaseMessageOptionFunc{
		courier.WithMessageMainPart(m.Content),
		courier.WithMessageSentAt(m.Timestamp),
	}

	for idx, attachment := range m.Attachments {
		funcs = append(funcs, courier.WithMessagePart(p.toAttachment(idx, attachment)))
	}

	if mentions := toMentions(m.Mentions); len(mentions) > 0 {
		funcs = append(funcs, courier.WithMessageMentions(mentions...))
	}

	if m.MessageReference != nil && m.MessageReference.MessageID != "" {
		funcs = append(funcs, courier.WithMessageInReplyTo(courier.MessageID(m.MessageReference.MessageID)))
	}

	return courier.NewMessage(
		courier.MessageID(m.ID),
		// Answer where the message was posted. Replying in a private channel
		// to something written in a guild channel would be surprising.
		p.channelOf(ctx, session, m),
		courier.NewUser(courier.UserID(m.Author.ID), m.Author.Username),
		funcs...,
	)
}

// toAttachment wraps a Discord attachment. Discord serves them over plain
// HTTP, so the content is fetched on first read only.
func (p *Provider) toAttachment(index int, attachment *discordgo.MessageAttachment) courier.Attachment {
	contentType := attachment.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	download := func(ctx context.Context) (io.ReadCloser, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, attachment.URL, nil)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		resp, err := p.opts.HTTPClient.Do(req)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, errors.Errorf("could not download attachment %q: unexpected status %d", attachment.Filename, resp.StatusCode)
		}

		return resp.Body, nil
	}

	open, release := courier.BufferedOpener(download, p.opts.MaxInMemorySize)
	p.trackRelease(release)

	funcs := []courier.AttachmentOptionFunc{
		courier.WithAttachmentName(partName(index)),
		courier.WithAttachmentSize(int64(attachment.Size)),
	}

	// Discord reports a duration for voice messages only.
	if attachment.DurationSecs > 0 {
		funcs = append(funcs, courier.WithAttachmentVoiceNote(
			time.Duration(attachment.DurationSecs*float64(time.Second)),
		))
	}

	return courier.NewAttachment(attachment.Filename, contentType, open, funcs...)
}

// Send implements courier.Provider.
func (p *Provider) Send(ctx context.Context, message courier.Message) error {
	session, err := p.getSession()
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

	send := &discordgo.MessageSend{
		Content: content,
		Files:   make([]*discordgo.File, 0, len(message.Parts())),
	}

	if parent, ok := courier.InReplyTo(message); ok {
		send.Reference = &discordgo.MessageReference{
			MessageID: string(parent),
			ChannelID: string(channel.ChannelID()),
		}
	}

	for _, attachment := range courier.Attachments(message) {
		reader, err := attachment.Reader(ctx)
		if err != nil {
			return errors.WithStack(err)
		}

		defer reader.Close()

		send.Files = append(send.Files, &discordgo.File{
			Name:        courier.FilenameFor(attachment),
			ContentType: attachment.ContentType(),
			Reader:      reader,
		})
	}

	if send.Content == "" && len(send.Files) == 0 {
		return errors.New("message has neither content nor attachment")
	}

	if _, err := session.ChannelMessageSendComplex(string(channel.ChannelID()), send); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// Self implements courier.SelfProvider.
func (p *Provider) Self(ctx context.Context) (courier.User, error) {
	session, err := p.getSession()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	user := session.State.User
	if user == nil {
		return nil, errors.WithStack(courier.ErrNotFound)
	}

	return courier.NewUser(courier.UserID(user.ID), user.Username), nil
}

// Channel implements courier.ChannelResolver.
func (p *Provider) Channel(ctx context.Context, channelID courier.ChannelID) (courier.Channel, error) {
	session, err := p.getSession()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	channel, err := session.Channel(string(channelID))
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return courier.NewChannel(channelID, kindOf(channel.Type), channel.Name), nil
}

// Capabilities implements courier.CapabilityProvider.
func (p *Provider) Capabilities() []courier.Capability {
	return []courier.Capability{
		courier.CapabilityReceiveAttachments,
		courier.CapabilitySendAttachments,
		courier.CapabilityChannelKind,
		courier.CapabilityMentions,
		courier.CapabilityThreads,
	}
}

// channelOf describes the channel a message was posted in, falling back on
// the guild identifier when the channel cannot be resolved.
func (p *Provider) channelOf(ctx context.Context, session *discordgo.Session, m *discordgo.MessageCreate) courier.Channel {
	channelID := courier.ChannelID(m.ChannelID)

	channel, err := session.State.Channel(m.ChannelID)
	if err != nil {
		slog.DebugContext(ctx, "could not resolve channel from state",
			slog.String("channelID", m.ChannelID), slog.Any("error", errors.WithStack(err)))

		// A message with a guild identifier was posted in a server channel,
		// which several people can read.
		kind := courier.ChannelKindDirect
		if m.GuildID != "" {
			kind = courier.ChannelKindPublic
		}

		return courier.NewChannel(channelID, kind, "")
	}

	return courier.NewChannel(channelID, kindOf(channel.Type), channel.Name)
}

func (p *Provider) getSession() (*discordgo.Session, error) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	if p.session == nil {
		return nil, errors.New("provider is not listening")
	}

	return p.session, nil
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

// kindOf classifies a Discord channel type.
func kindOf(channelType discordgo.ChannelType) courier.ChannelKind {
	switch channelType {
	case discordgo.ChannelTypeDM:
		return courier.ChannelKindDirect
	case discordgo.ChannelTypeGroupDM, discordgo.ChannelTypeGuildPrivateThread:
		return courier.ChannelKindGroup
	case discordgo.ChannelTypeGuildText, discordgo.ChannelTypeGuildNews,
		discordgo.ChannelTypeGuildPublicThread, discordgo.ChannelTypeGuildNewsThread,
		discordgo.ChannelTypeGuildForum:
		return courier.ChannelKindPublic
	default:
		return courier.ChannelKindUnknown
	}
}

func toMentions(users []*discordgo.User) []courier.Mention {
	mentions := make([]courier.Mention, 0, len(users))

	for _, user := range users {
		mentions = append(mentions, courier.Mention{
			UserID:      courier.UserID(user.ID),
			DisplayName: user.Username,
		})
	}

	return mentions
}

func partName(index int) string {
	return "att-" + strconv.Itoa(index)
}

func NewProvider(funcs ...OptionFunc) *Provider {
	return &Provider{
		opts:    NewOptions(funcs...),
		release: []courier.CloseFunc{},
	}
}

var (
	_ courier.Provider           = &Provider{}
	_ courier.SelfProvider       = &Provider{}
	_ courier.ChannelResolver    = &Provider{}
	_ courier.CapabilityProvider = &Provider{}
)
