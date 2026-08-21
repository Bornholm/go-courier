package whatsapp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/syncx"
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

	// The WASM binary now ships with the driver itself: importing embed as
	// well triggers a "you're unnecessarily importing" notice on stderr at
	// startup.
	_ "github.com/ncruces/go-sqlite3/driver"
)

func init() {
	store.DeviceProps.Os = proto.String("go-courier")
}

type Provider struct {
	opts *Options

	initErr  error
	initOnce sync.Once
	client   *whatsmeow.Client

	// groupNames caches group names, since resolving one is a network call.
	groupNames syncx.Map[types.JID, string]

	// expirations remembers the disappearing-messages setting observed on
	// each chat, so that replies mirror it instead of imposing one.
	expirations syncx.Map[types.JID, uint32]

	releaseMutex sync.Mutex
	release      []courier.CloseFunc
}

// SetPresence implements courier.PresenceProvider.
func (p *Provider) SetPresence(ctx context.Context, presence courier.Presence) error {
	client, err := p.getClient(ctx)
	if err != nil {
		return errors.WithStack(err)
	}

	switch presence {
	case courier.PresenceOnline:
		if err := client.SendPresence(ctx, types.PresenceAvailable); err != nil {
			return errors.WithStack(err)
		}
	case courier.PresenceOffline:
		if err := client.SendPresence(ctx, types.PresenceUnavailable); err != nil {
			return errors.WithStack(err)
		}
	}

	return nil
}

// SetStatus implements courier.StatusProvider.
func (p *Provider) SetStatus(ctx context.Context, status courier.Status, channelID courier.ChannelID) error {
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
		if err := client.SendChatPresence(ctx, jid, types.ChatPresenceComposing, types.ChatPresenceMediaText); err != nil {
			return errors.WithStack(err)
		}
	case courier.StatusIdle:
		if err := client.SendChatPresence(ctx, jid, types.ChatPresencePaused, types.ChatPresenceMediaText); err != nil {
			return errors.WithStack(err)
		}
	}

	return nil
}

// Listen implements courier.Provider.
//
// Every message the account receives is forwarded, groups and media
// included. Deciding whether to answer is the application's call: use
// Channel().Kind() and courier.IsMentioned to filter.
func (p *Provider) Listen(ctx context.Context) (chan courier.Message, error) {
	client, err := p.getClient(ctx)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if err := client.SendPresence(ctx, types.PresenceAvailable); err != nil {
		return nil, errors.WithStack(err)
	}

	messages := make(chan courier.Message)

	client.AddEventHandler(func(evt any) {
		event, ok := evt.(*events.Message)
		if !ok {
			return
		}

		// Learned even from our own messages: the setting belongs to the
		// chat, not to whoever happens to write in it.
		p.rememberExpiration(event)

		if event.Info.MessageSource.IsFromMe {
			return
		}

		message := p.toMessage(ctx, client, event)
		if message == nil {
			return
		}

		select {
		case messages <- message:
		case <-ctx.Done():
		}
	})

	go func() {
		<-ctx.Done()
		p.releaseAll()
	}()

	return messages, nil
}

// toMessage converts a WhatsApp event, returning nil when it carries neither
// text nor media worth forwarding.
func (p *Provider) toMessage(ctx context.Context, client *whatsmeow.Client, event *events.Message) courier.Message {
	text := textOf(event.Message)
	media, hasMedia := mediaOf(event.Message)

	if text == "" && !hasMedia {
		return nil
	}

	chat := event.Info.MessageSource.Chat

	funcs := []courier.BaseMessageOptionFunc{
		courier.WithMessageSentAt(event.Info.Timestamp),
	}

	if hasMedia {
		attachment := p.toAttachment(client, media)
		funcs = append(funcs, courier.WithMessagePart(attachment))

		// A media message with no text of its own is described by its
		// caption, which is what the sender actually typed.
		if text == "" {
			text = media.caption
		}
	}

	funcs = append(funcs, courier.WithMessageMainPart(text))

	contextInfo := contextInfoOf(event.Message)

	if contextInfo != nil {
		if mentions := toMentions(ctx, client, contextInfo.GetMentionedJID()); len(mentions) > 0 {
			funcs = append(funcs, courier.WithMessageMentions(mentions...))
		}

		if stanzaID := contextInfo.GetStanzaID(); stanzaID != "" {
			funcs = append(funcs, courier.WithMessageInReplyTo(courier.MessageID(stanzaID)))
		}
	}

	return courier.NewMessage(
		courier.MessageID(event.Info.ID),
		p.channelOf(ctx, client, chat),
		courier.NewUser(userIDOf(event.Info.MessageSource.Sender), event.Info.PushName),
		funcs...,
	)
}

// Send implements courier.Provider.
//
// WhatsApp carries at most one media per message, so a message with several
// attachments is split: the text rides along as the caption of the first one.
func (p *Provider) Send(ctx context.Context, message courier.Message) error {
	client, err := p.getClient(ctx)
	if err != nil {
		return errors.WithStack(err)
	}

	channel := message.Channel()
	if channel == nil {
		return errors.New("message has no channel")
	}

	to, err := types.ParseJID(string(channel.ChannelID()))
	if err != nil {
		return errors.WithStack(err)
	}

	content, err := courier.GetMessageMainContent(ctx, message)
	if err != nil && !errors.Is(err, courier.ErrNotFound) {
		return errors.WithStack(err)
	}

	attachments := courier.Attachments(message)

	slog.DebugContext(ctx, "sending message",
		slog.String("channelID", string(channel.ChannelID())),
		slog.Int("attachments", len(attachments)),
	)

	if len(attachments) == 0 {
		if content == "" {
			return errors.New("message has neither content nor attachment")
		}

		return errors.WithStack(p.sendText(ctx, client, to, content))
	}

	for idx, attachment := range attachments {
		// Only the first media carries the text, otherwise it would be
		// repeated under every file.
		caption := ""
		if idx == 0 {
			caption = content
		}

		payload, err := buildMediaMessage(ctx, client, attachment, caption)
		if err != nil {
			return errors.WithStack(err)
		}

		p.applyExpiration(payload, to)

		if _, err := client.SendMessage(ctx, to, payload); err != nil {
			return errors.WithStack(err)
		}
	}

	return nil
}

func (p *Provider) sendText(ctx context.Context, client *whatsmeow.Client, to types.JID, content string) error {
	payload := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(content),
		},
	}
	p.applyExpiration(payload, to)

	_, err := client.SendMessage(ctx, to, payload)

	return errors.WithStack(err)
}

// rememberExpiration records the disappearing-messages setting carried by an
// incoming message, including its absence: a chat whose timer was turned off
// must stop receiving expiring replies.
func (p *Provider) rememberExpiration(event *events.Message) {
	expiration := contextInfoOf(event.Message).GetExpiration()

	// A message unwrapped from an EphemeralMessage always belongs to a
	// disappearing chat, even when its context info says nothing.
	if expiration == 0 && event.IsEphemeral {
		return
	}

	p.expirations.Store(event.Info.MessageSource.Chat, expiration)
}

// applyExpiration marks an outgoing message with the lifetime of its chat, so
// that a reply disappears exactly like what it answers — no more, no less.
// Nothing is marked when the chat keeps its messages.
func (p *Provider) applyExpiration(msg *waE2E.Message, to types.JID) {
	expiration, known := p.expirations.Load(to)
	if !known {
		expiration = uint32(p.opts.DisappearingTimer.Seconds())
	}

	if expiration == 0 {
		return
	}

	contextInfo := contextInfoFor(msg)
	if contextInfo == nil {
		return
	}

	contextInfo.Expiration = proto.Uint32(expiration)
}

// Self implements courier.SelfProvider.
func (p *Provider) Self(ctx context.Context) (courier.User, error) {
	client, err := p.getClient(ctx)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if client.Store.ID == nil {
		return nil, errors.WithStack(courier.ErrNotFound)
	}

	return courier.NewUser(userIDOf(*client.Store.ID), client.Store.PushName), nil
}

// userIDOf builds the stable identity of a WhatsApp user, stripping the
// device part of the JID.
//
// WhatsApp addresses every linked device separately: the same person appears
// as "<user>@lid" from their primary phone and as "<user>:22@lid" from their
// 22nd linked device (WhatsApp Web, a second phone). Keeping that suffix
// would make the identity depend on which device happened to send the
// message, and any table keyed by user — an allow list, a mapping to an
// account — would silently miss messages sent from anywhere else.
func userIDOf(jid types.JID) courier.UserID {
	return courier.UserID(jid.ToNonAD().String())
}

// Channel implements courier.ChannelResolver.
func (p *Provider) Channel(ctx context.Context, channelID courier.ChannelID) (courier.Channel, error) {
	client, err := p.getClient(ctx)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	jid, err := types.ParseJID(string(channelID))
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return p.channelOf(ctx, client, jid), nil
}

// Capabilities implements courier.CapabilityProvider.
func (p *Provider) Capabilities() []courier.Capability {
	return []courier.Capability{
		courier.CapabilityReceiveAttachments,
		courier.CapabilitySendAttachments,
		courier.CapabilityChannelKind,
		courier.CapabilityMentions,
		courier.CapabilityThreads,
		courier.CapabilityPresence,
		courier.CapabilityStatus,
	}
}

// channelOf describes a chat, resolving group names lazily and caching them.
func (p *Provider) channelOf(ctx context.Context, client *whatsmeow.Client, jid types.JID) courier.Channel {
	kind := kindOf(jid)

	name := jid.User
	if kind == courier.ChannelKindGroup {
		name = p.groupName(ctx, client, jid)
	}

	return courier.NewChannel(courier.ChannelID(jid.String()), kind, name)
}

func (p *Provider) groupName(ctx context.Context, client *whatsmeow.Client, jid types.JID) string {
	if name, exists := p.groupNames.Load(jid); exists {
		return name
	}

	info, err := client.GetGroupInfo(ctx, jid)
	if err != nil {
		slog.DebugContext(ctx, "could not resolve group name",
			slog.String("jid", jid.String()), slog.Any("error", errors.WithStack(err)))

		return jid.User
	}

	p.groupNames.Store(jid, info.Name)

	return info.Name
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
			slog.Error("could not release media content", slog.Any("error", errors.WithStack(err)))
		}
	}
}

func (p *Provider) getClient(ctx context.Context) (*whatsmeow.Client, error) {
	p.initOnce.Do(func() {
		slog.DebugContext(ctx, "initializing whatsapp client")

		dbLog := waLog.Stdout("Database", "DEBUG", true)
		container, err := sqlstore.New(ctx, "sqlite3", fmt.Sprintf("%s?_foreign_keys=on", p.opts.DBPath), dbLog)
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

		client.Store.PushName = p.opts.PushName

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
				switch {
				case evt.Event == "code" && p.opts.QRHandler != nil:
					p.opts.QRHandler(ctx, evt.Code, false)
				case evt.Event == "code":
					qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
				default:
					slog.DebugContext(ctx, "whatsapp client logged in", slog.String("event", evt.Event))
					if p.opts.QRHandler != nil {
						p.opts.QRHandler(ctx, "", evt.Event == "success")
					}
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

// kindOf classifies a chat from the server part of its JID.
func kindOf(jid types.JID) courier.ChannelKind {
	switch jid.Server {
	case types.GroupServer:
		return courier.ChannelKindGroup
	case types.DefaultUserServer, types.HiddenUserServer, types.LegacyUserServer, types.BotServer:
		return courier.ChannelKindDirect
	case types.NewsletterServer, types.BroadcastServer:
		return courier.ChannelKindPublic
	default:
		return courier.ChannelKindUnknown
	}
}

// textOf extracts the textual content of a message, whatever type carries it.
func textOf(msg *waE2E.Message) string {
	if msg == nil {
		return ""
	}

	switch {
	case msg.GetExtendedTextMessage() != nil:
		return msg.GetExtendedTextMessage().GetText()
	case msg.GetConversation() != "":
		return msg.GetConversation()
	default:
		return ""
	}
}

// contextInfoOf returns the context info of a message, holding mentions and
// the quoted message identifier.
func contextInfoOf(msg *waE2E.Message) *waE2E.ContextInfo {
	if msg == nil {
		return nil
	}

	switch {
	case msg.GetExtendedTextMessage() != nil:
		return msg.GetExtendedTextMessage().GetContextInfo()
	case msg.GetImageMessage() != nil:
		return msg.GetImageMessage().GetContextInfo()
	case msg.GetAudioMessage() != nil:
		return msg.GetAudioMessage().GetContextInfo()
	case msg.GetVideoMessage() != nil:
		return msg.GetVideoMessage().GetContextInfo()
	case msg.GetDocumentMessage() != nil:
		return msg.GetDocumentMessage().GetContextInfo()
	case msg.GetStickerMessage() != nil:
		return msg.GetStickerMessage().GetContextInfo()
	default:
		return nil
	}
}

// contextInfoFor returns the context info of an outgoing message, creating it
// when the message type carries one but has none yet. It returns nil for the
// types that have no context info at all.
func contextInfoFor(msg *waE2E.Message) *waE2E.ContextInfo {
	if msg == nil {
		return nil
	}

	ensure := func(current *waE2E.ContextInfo, set func(*waE2E.ContextInfo)) *waE2E.ContextInfo {
		if current == nil {
			current = &waE2E.ContextInfo{}
			set(current)
		}

		return current
	}

	switch {
	case msg.GetExtendedTextMessage() != nil:
		m := msg.GetExtendedTextMessage()
		return ensure(m.GetContextInfo(), func(ci *waE2E.ContextInfo) { m.ContextInfo = ci })
	case msg.GetImageMessage() != nil:
		m := msg.GetImageMessage()
		return ensure(m.GetContextInfo(), func(ci *waE2E.ContextInfo) { m.ContextInfo = ci })
	case msg.GetAudioMessage() != nil:
		m := msg.GetAudioMessage()
		return ensure(m.GetContextInfo(), func(ci *waE2E.ContextInfo) { m.ContextInfo = ci })
	case msg.GetVideoMessage() != nil:
		m := msg.GetVideoMessage()
		return ensure(m.GetContextInfo(), func(ci *waE2E.ContextInfo) { m.ContextInfo = ci })
	case msg.GetDocumentMessage() != nil:
		m := msg.GetDocumentMessage()
		return ensure(m.GetContextInfo(), func(ci *waE2E.ContextInfo) { m.ContextInfo = ci })
	case msg.GetStickerMessage() != nil:
		m := msg.GetStickerMessage()
		return ensure(m.GetContextInfo(), func(ci *waE2E.ContextInfo) { m.ContextInfo = ci })
	default:
		return nil
	}
}

// toMentions converts mentioned JIDs to courier mentions. In groups using
// LID addressing, ContextInfo.MentionedJID carries "...@lid" JIDs while
// Self() exposes the phone-number JID: without translation, a mention of the
// bot would never match courier.IsMentioned. Each JID whose alternative form
// (LID <-> phone number) is known to the device store therefore yields a
// second, equivalent mention, so the comparison works whichever form the
// caller holds.
func toMentions(ctx context.Context, client *whatsmeow.Client, jids []string) []courier.Mention {
	mentions := make([]courier.Mention, 0, len(jids))

	for _, raw := range jids {
		jid, err := types.ParseJID(raw)
		if err != nil {
			continue
		}

		mentions = append(mentions, courier.Mention{
			UserID:      courier.UserID(jid.String()),
			DisplayName: jid.User,
		})

		if client == nil || client.Store == nil {
			continue
		}

		// The bot's own LID <-> phone-number pair is known statically on the
		// device (Store.LID / Store.ID, set at pairing): resolve it directly.
		// Going through the LID store would miss right after startup, before
		// the store is populated — and a missed mention of the bot itself is
		// precisely the case that must never happen (group mention rule).
		if !client.Store.LID.IsEmpty() && jid.ToNonAD().User == client.Store.LID.User && client.Store.ID != nil {
			pn := client.Store.ID.ToNonAD()
			mentions = append(mentions, courier.Mention{
				UserID:      courier.UserID(pn.String()),
				DisplayName: pn.User,
			})
			continue
		}

		alt, err := client.Store.GetAltJID(ctx, jid.ToNonAD())
		if err != nil || alt.IsEmpty() {
			continue
		}

		mentions = append(mentions, courier.Mention{
			UserID:      courier.UserID(alt.ToNonAD().String()),
			DisplayName: alt.User,
		})
	}

	return mentions
}

func NewProvider(funcs ...OptionFunc) *Provider {
	return &Provider{
		opts:    NewOptions(funcs...),
		release: []courier.CloseFunc{},
	}
}

var (
	_ courier.Provider           = &Provider{}
	_ courier.PresenceProvider   = &Provider{}
	_ courier.StatusProvider     = &Provider{}
	_ courier.SelfProvider       = &Provider{}
	_ courier.ChannelResolver    = &Provider{}
	_ courier.CapabilityProvider = &Provider{}
)
