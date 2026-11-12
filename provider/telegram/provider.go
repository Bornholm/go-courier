// Package telegram is a courier provider for Telegram bots, over the Bot
// API.
//
// Updates are received by long polling (getUpdates): no public endpoint is
// needed, but the bot must have no webhook set, and a single process may
// poll a given bot at a time — Telegram answers a second poller with a 409.
//
// Identifiers:
//   - a channel is a chat, "<chat_id>"; a topic of a forum supergroup is
//     a channel of its own, "<chat_id>/<message_thread_id>", so that an
//     answer lands in the topic it answers rather than in General;
//   - a message is "<chat_id>:<message_id>", Telegram message identifiers
//     being unique within a chat only;
//   - a user is its numeric Telegram identifier. In a private chat, the
//     chat identifier equals the user's.
package telegram

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/bornholm/go-courier"
	"github.com/pkg/errors"
)

const (
	// maxTextLength is the Bot API limit for a text message, in UTF-16
	// code units.
	maxTextLength = 4096
	// maxCaptionLength is the Bot API limit for a media caption.
	maxCaptionLength = 1024
)

// allowedUpdates are the update kinds the provider asks for. Reactions are
// never delivered unless explicitly requested.
var allowedUpdates = []string{"message", "message_reaction"}

type Provider struct {
	opts *Options

	mu        sync.Mutex
	listening bool
	self      *apiUser

	reactMu   sync.Mutex
	reactions chan courier.Reaction

	releaseMu sync.Mutex
	release   []courier.CloseFunc
}

func NewProvider(funcs ...OptionFunc) *Provider {
	return &Provider{
		opts: NewOptions(funcs...),
	}
}

// Listen implements courier.Provider.
//
// It checks the token with getMe before returning, then polls for updates
// until ctx is done. Every message the bot receives is forwarded, groups
// and media included; filtering is the application's call. In groups, a bot
// with privacy mode on (the default) only receives commands, replies to its
// own messages and messages mentioning it.
func (p *Provider) Listen(ctx context.Context) (chan courier.Message, error) {
	p.mu.Lock()
	if p.listening {
		p.mu.Unlock()
		return nil, errors.New("provider is already listening")
	}
	p.listening = true
	p.mu.Unlock()

	if _, err := p.getMe(ctx); err != nil {
		p.mu.Lock()
		p.listening = false
		p.mu.Unlock()
		return nil, errors.WithStack(err)
	}

	messages := make(chan courier.Message)

	go func() {
		defer close(messages)
		defer p.releaseAll()
		defer func() {
			p.mu.Lock()
			p.listening = false
			p.mu.Unlock()
		}()

		p.poll(ctx, messages)
	}()

	return messages, nil
}

// poll runs the getUpdates loop until ctx is done.
func (p *Provider) poll(ctx context.Context, messages chan courier.Message) {
	var (
		offset    int64
		confirmed int64
		delay     time.Duration
	)

	// Telegram forgets an update once a later getUpdates call asks for an
	// offset past it. Without this last call, the updates handled since the
	// previous poll would be delivered again on restart.
	defer func() {
		if offset == confirmed {
			return
		}

		confirmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()

		payload := map[string]any{"offset": offset, "limit": 1, "timeout": 0, "allowed_updates": allowedUpdates}
		if err := p.call(confirmCtx, "getUpdates", payload, nil); err != nil {
			slog.WarnContext(ctx, "telegram: could not confirm the last handled updates", slog.Any("error", err))
		}
	}()

	for {
		if ctx.Err() != nil {
			return
		}

		payload := map[string]any{
			"offset":          offset,
			"timeout":         int(p.opts.PollTimeout / time.Second),
			"allowed_updates": allowedUpdates,
		}

		var updates []apiUpdate
		err := p.call(ctx, "getUpdates", payload, &updates)
		if err != nil {
			if ctx.Err() != nil {
				return
			}

			delay = p.nextDelay(delay, err)
			slog.WarnContext(ctx, "telegram: could not poll updates",
				slog.Any("error", err), slog.Duration("retry_in", delay))

			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}

			continue
		}

		delay = 0
		confirmed = offset

		for _, update := range updates {
			if !p.dispatch(ctx, update, messages) {
				return
			}
			offset = update.UpdateID + 1
		}
	}
}

// nextDelay doubles the delay between failed polls, honouring the delay
// Telegram asks for when rate-limiting.
func (p *Provider) nextDelay(previous time.Duration, err error) time.Duration {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
		return time.Duration(apiErr.RetryAfter) * time.Second
	}

	next := previous * 2
	if next < time.Second {
		next = time.Second
	}
	if next > p.opts.MaxRetryDelay {
		next = p.opts.MaxRetryDelay
	}
	return next
}

// dispatch hands an update over. It reports false when ctx ended before the
// update could be delivered.
func (p *Provider) dispatch(ctx context.Context, update apiUpdate, messages chan courier.Message) bool {
	switch {
	case update.Message != nil:
		message := p.toMessage(ctx, update.Message)
		if message == nil {
			return true
		}

		select {
		case messages <- message:
			return true
		case <-ctx.Done():
			return false
		}

	case update.MessageReaction != nil:
		p.emitReaction(ctx, update.MessageReaction)
	}

	return true
}

func (p *Provider) toMessage(ctx context.Context, raw *apiMessage) courier.Message {
	text, entities := raw.Text, raw.Entities
	if text == "" {
		text, entities = raw.Caption, raw.CaptionEntities
	}

	attachments := p.attachmentsOf(raw, text)

	// Stickers, locations, polls, service messages (someone joined, the
	// title changed…) carry nothing an application can read.
	if text == "" && len(attachments) == 0 {
		return nil
	}

	funcs := []courier.BaseMessageOptionFunc{
		courier.WithMessageSentAt(time.Unix(raw.Date, 0)),
	}
	if text != "" {
		funcs = append(funcs, courier.WithMessageMainPart(text))
	}
	for _, attachment := range attachments {
		funcs = append(funcs, courier.WithMessagePart(attachment))
	}

	if mentions := p.mentionsOf(ctx, text, entities); len(mentions) > 0 {
		funcs = append(funcs, courier.WithMessageMentions(mentions...))
	}

	// In a forum, every topic message replies to the message that opened
	// the topic: that is how Telegram threads them, not something the user
	// answered, and the topic already shows in the channel.
	if parent := raw.ReplyToMessage; parent != nil && !(raw.IsTopicMessage && parent.MessageID == raw.MessageThreadID) {
		funcs = append(funcs, courier.WithMessageInReplyTo(messageID(raw.Chat.ID, parent.MessageID)))
	}

	return courier.NewMessage(
		messageID(raw.Chat.ID, raw.MessageID),
		channelOf(raw),
		senderOf(raw),
		funcs...,
	)
}

// attachmentsOf wraps the files carried by a message. Their content is only
// downloaded when read.
func (p *Provider) attachmentsOf(raw *apiMessage, caption string) []courier.Attachment {
	var attachments []courier.Attachment

	add := func(fileID, filename, contentType string, size int64, extra ...courier.AttachmentOptionFunc) {
		if contentType == "" {
			contentType = "application/octet-stream"
		}

		open, release := courier.BufferedOpener(func(ctx context.Context) (io.ReadCloser, error) {
			return p.download(ctx, fileID)
		}, p.opts.MaxInMemorySize)
		p.trackRelease(release)

		funcs := []courier.AttachmentOptionFunc{
			courier.WithAttachmentName("att-" + strconv.Itoa(len(attachments))),
			courier.WithAttachmentCaption(caption),
		}
		if size > 0 {
			funcs = append(funcs, courier.WithAttachmentSize(size))
		}
		funcs = append(funcs, extra...)

		attachments = append(attachments, courier.NewAttachment(filename, contentType, open, funcs...))
	}

	// A photo comes in several sizes, smallest first: only the largest is
	// worth reading.
	if n := len(raw.Photo); n > 0 {
		photo := raw.Photo[n-1]
		add(photo.FileID, "photo.jpg", "image/jpeg", photo.FileSize)
	}

	if f := raw.Voice; f != nil {
		add(f.FileID, "voice.ogg", orDefault(f.MimeType, "audio/ogg"), f.FileSize,
			courier.WithAttachmentVoiceNote(seconds(f.Duration)))
	}

	if f := raw.Audio; f != nil {
		add(f.FileID, orDefault(f.FileName, "audio.mp3"), orDefault(f.MimeType, "audio/mpeg"), f.FileSize,
			courier.WithAttachmentDuration(seconds(f.Duration)))
	}

	if f := raw.Video; f != nil {
		add(f.FileID, orDefault(f.FileName, "video.mp4"), orDefault(f.MimeType, "video/mp4"), f.FileSize,
			courier.WithAttachmentDuration(seconds(f.Duration)))
	}

	if f := raw.VideoNote; f != nil {
		add(f.FileID, "video_note.mp4", "video/mp4", f.FileSize,
			courier.WithAttachmentDuration(seconds(f.Duration)))
	}

	// An animation (a GIF, converted to MP4 by Telegram) also comes with a
	// document field describing the same file: reading it once is enough.
	if f := raw.Animation; f != nil {
		add(f.FileID, orDefault(f.FileName, "animation.mp4"), orDefault(f.MimeType, "video/mp4"), f.FileSize,
			courier.WithAttachmentDuration(seconds(f.Duration)))
	} else if f := raw.Document; f != nil {
		add(f.FileID, f.FileName, f.MimeType, f.FileSize)
	}

	return attachments
}

// mentionsOf reports the users named in a message. A "text_mention" carries
// the user; a "mention" only carries an @username, which is resolved for
// the bot itself only — Telegram offers no way to resolve anyone else's,
// and an application only needs to know whether it was addressed. A command
// suffixed with the bot username ("/start@my_bot") addresses it as well.
func (p *Provider) mentionsOf(ctx context.Context, text string, entities []apiEntity) []courier.Mention {
	if len(entities) == 0 {
		return nil
	}

	self, err := p.getMe(ctx)
	if err != nil {
		slog.WarnContext(ctx, "telegram: could not identify the bot, mentions of it are lost", slog.Any("error", err))
	}

	units := utf16.Encode([]rune(text))

	var mentions []courier.Mention

	for _, entity := range entities {
		switch entity.Type {
		case "text_mention":
			if entity.User != nil {
				mentions = append(mentions, courier.Mention{
					UserID:      userID(entity.User.ID),
					DisplayName: displayName(entity.User),
				})
			}

		case "mention", "bot_command":
			if self == nil || entity.Offset < 0 || entity.Length <= 0 || entity.Offset+entity.Length > len(units) {
				continue
			}

			value := string(utf16.Decode(units[entity.Offset : entity.Offset+entity.Length]))

			_, username, found := strings.Cut(value, "@")
			if !found || !strings.EqualFold(username, self.Username) {
				continue
			}

			mentions = append(mentions, courier.Mention{
				UserID:      userID(self.ID),
				DisplayName: displayName(self),
			})
		}
	}

	return mentions
}

func (p *Provider) emitReaction(ctx context.Context, raw *apiReactionUpdated) {
	p.reactMu.Lock()
	defer p.reactMu.Unlock()

	if p.reactions == nil {
		return
	}

	var from courier.User
	switch {
	case raw.User != nil:
		from = courier.NewUser(userID(raw.User.ID), displayName(raw.User))
	case raw.ActorChat != nil:
		from = courier.NewUser(userID(raw.ActorChat.ID), chatName(raw.ActorChat))
	default:
		// Anonymous reactions are only reported as counts, by another
		// update the provider does not ask for.
		return
	}

	// Telegram reports the whole set of reactions a user left on the
	// message. Courier carries one emoji: the first one, as custom and
	// paid reactions have no emoji to show. An empty set takes the
	// reaction back.
	var emoji string
	for _, reaction := range raw.NewReaction {
		if reaction.Type == "emoji" && reaction.Emoji != "" {
			emoji = reaction.Emoji
			break
		}
	}
	if emoji == "" && len(raw.NewReaction) > 0 {
		return
	}

	reaction := courier.Reaction{
		MessageID: messageID(raw.Chat.ID, raw.MessageID),
		Channel:   courier.NewChannel(chatChannelID(raw.Chat.ID), kindOf(raw.Chat.Type), chatName(&raw.Chat)),
		From:      from,
		Emoji:     emoji,
		ReactedAt: time.Unix(raw.Date, 0),
	}

	select {
	case p.reactions <- reaction:
	default:
		slog.WarnContext(ctx, "telegram: reaction dropped, nobody reads the reaction channel")
	}
}

// ListenReactions implements courier.ReactionProvider.
//
// Reactions travel on the same updates as messages: they only flow while
// Listen runs. Telegram reports reactions in private chats, and in groups
// where the bot is an administrator.
func (p *Provider) ListenReactions(ctx context.Context) (chan courier.Reaction, error) {
	p.reactMu.Lock()
	defer p.reactMu.Unlock()

	if p.reactions != nil {
		return nil, errors.New("provider is already listening for reactions")
	}

	reactions := make(chan courier.Reaction, 16)
	p.reactions = reactions

	go func() {
		<-ctx.Done()

		p.reactMu.Lock()
		defer p.reactMu.Unlock()

		p.reactions = nil
		close(reactions)
	}()

	return reactions, nil
}

// Send implements courier.Provider.
//
// A text longer than Telegram allows is split into several messages. When
// the message carries files, a short text travels as the caption of the
// first one; a longer text is sent first, on its own. The returned
// identifier is the first message sent, the one replying to InReplyTo.
func (p *Provider) Send(ctx context.Context, message courier.Message) (courier.MessageID, error) {
	channel := message.Channel()
	if channel == nil {
		return "", errors.New("message has no channel")
	}

	chatID, threadID, err := parseChannelID(channel.ChannelID())
	if err != nil {
		return "", errors.WithStack(err)
	}

	text, err := courier.GetMessageMainContent(ctx, message)
	if err != nil && !errors.Is(err, courier.ErrNotFound) {
		return "", errors.WithStack(err)
	}

	attachments := courier.Attachments(message)

	if text == "" && len(attachments) == 0 {
		return "", errors.New("message has neither content nor attachment")
	}

	var reply *replyParameters
	if parent, ok := courier.InReplyTo(message); ok {
		id, err := parseMessageID(parent)
		if err != nil {
			// The message still goes, as a plain one: losing the reply link
			// is better than losing the answer.
			slog.WarnContext(ctx, "telegram: reply target is not a telegram message id, sending without reply",
				slog.String("in_reply_to", string(parent)))
		} else {
			reply = &replyParameters{MessageID: id, AllowSendingWithoutReply: true}
		}
	}

	var first courier.MessageID

	sent := func(id int) {
		if first == "" {
			first = messageID(chatID, id)
			// Only the first message answers: the others follow it.
			reply = nil
		}
	}

	caption := ""
	if len(attachments) > 0 && utf16Len(text) <= maxCaptionLength {
		caption, text = text, ""
	}

	for _, chunk := range splitText(text, maxTextLength) {
		payload := map[string]any{"chat_id": chatID, "text": chunk}
		if threadID != 0 {
			payload["message_thread_id"] = threadID
		}
		if reply != nil {
			payload["reply_parameters"] = reply
		}

		var result apiMessage
		if err := p.call(ctx, "sendMessage", payload, &result); err != nil {
			return first, errors.WithStack(err)
		}
		sent(result.MessageID)
	}

	for idx, attachment := range attachments {
		fileCaption := ""
		if idx == 0 {
			fileCaption = caption
		}

		id, err := p.sendAttachment(ctx, chatID, threadID, reply, attachment, fileCaption)
		if err != nil {
			return first, errors.WithStack(err)
		}
		sent(id)
	}

	return first, nil
}

// sendAttachment sends a single file with the method matching its kind, so
// that it displays as Telegram users expect: a photo inline, a voice note
// as a playable bubble.
func (p *Provider) sendAttachment(ctx context.Context, chatID int64, threadID int, reply *replyParameters, attachment courier.Attachment, caption string) (int, error) {
	method, field := methodFor(attachment)

	fields := map[string]string{"chat_id": strconv.FormatInt(chatID, 10)}
	if threadID != 0 {
		fields["message_thread_id"] = strconv.Itoa(threadID)
	}
	if caption != "" {
		fields["caption"] = caption
	}
	if reply != nil {
		fields["reply_parameters"] = fmt.Sprintf(`{"message_id":%d,"allow_sending_without_reply":true}`, reply.MessageID)
	}

	reader, err := attachment.Reader(ctx)
	if err != nil {
		return 0, errors.WithStack(err)
	}
	defer reader.Close()

	var result apiMessage
	err = p.callMultipart(ctx, method, fields, upload{
		field:       field,
		filename:    courier.FilenameFor(attachment),
		contentType: attachment.ContentType(),
		content:     reader,
	}, &result)
	if err != nil {
		return 0, errors.WithStack(err)
	}

	return result.MessageID, nil
}

// methodFor picks the Bot API method sending an attachment, and the name of
// its file field. Formats Telegram does not display natively go as plain
// documents, which accept anything.
func methodFor(attachment courier.Attachment) (method, field string) {
	essence := strings.ToLower(strings.TrimSpace(strings.SplitN(attachment.ContentType(), ";", 2)[0]))

	switch {
	case courier.IsVoiceNote(attachment) && courier.MediaKindOf(essence) == courier.MediaKindAudio:
		return "sendVoice", "voice"
	case essence == "image/jpeg", essence == "image/png", essence == "image/webp":
		return "sendPhoto", "photo"
	case essence == "audio/mpeg", essence == "audio/mp4", essence == "audio/m4a", essence == "audio/x-m4a":
		return "sendAudio", "audio"
	case essence == "video/mp4":
		return "sendVideo", "video"
	default:
		return "sendDocument", "document"
	}
}

// SetStatus implements courier.StatusProvider. Telegram shows the typing
// indicator for five seconds, or until the bot sends a message: there is
// nothing to clear when going idle.
func (p *Provider) SetStatus(ctx context.Context, status courier.Status, channelID courier.ChannelID) error {
	if status != courier.StatusTyping {
		return nil
	}

	chatID, threadID, err := parseChannelID(channelID)
	if err != nil {
		return errors.WithStack(err)
	}

	payload := map[string]any{"chat_id": chatID, "action": "typing"}
	if threadID != 0 {
		payload["message_thread_id"] = threadID
	}

	return errors.WithStack(p.call(ctx, "sendChatAction", payload, nil))
}

// Self implements courier.SelfProvider.
func (p *Provider) Self(ctx context.Context) (courier.User, error) {
	self, err := p.getMe(ctx)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return courier.NewUser(userID(self.ID), displayName(self)), nil
}

// getMe returns the bot account, asking Telegram the first time only.
func (p *Provider) getMe(ctx context.Context) (*apiUser, error) {
	p.mu.Lock()
	self := p.self
	p.mu.Unlock()

	if self != nil {
		return self, nil
	}

	var user apiUser
	if err := p.call(ctx, "getMe", map[string]any{}, &user); err != nil {
		return nil, errors.WithStack(err)
	}

	p.mu.Lock()
	p.self = &user
	p.mu.Unlock()

	return &user, nil
}

// Channel implements courier.ChannelResolver.
func (p *Provider) Channel(ctx context.Context, channelID courier.ChannelID) (courier.Channel, error) {
	chatID, _, err := parseChannelID(channelID)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	var chat apiChat
	if err := p.call(ctx, "getChat", map[string]any{"chat_id": chatID}, &chat); err != nil {
		return nil, errors.WithStack(err)
	}

	return courier.NewChannel(channelID, kindOf(chat.Type), chatName(&chat)), nil
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
		courier.CapabilityReactions,
	}
}

func (p *Provider) trackRelease(release courier.CloseFunc) {
	p.releaseMu.Lock()
	defer p.releaseMu.Unlock()

	p.release = append(p.release, release)
}

func (p *Provider) releaseAll() {
	p.releaseMu.Lock()
	release := p.release
	p.release = nil
	p.releaseMu.Unlock()

	for _, fn := range release {
		if err := fn(); err != nil {
			slog.Error("telegram: could not release attachment content", slog.Any("error", errors.WithStack(err)))
		}
	}
}

// channelOf describes the channel a message was posted in.
func channelOf(raw *apiMessage) courier.Channel {
	id := chatChannelID(raw.Chat.ID)
	if raw.IsTopicMessage && raw.MessageThreadID != 0 {
		id = courier.ChannelID(fmt.Sprintf("%s/%d", id, raw.MessageThreadID))
	}

	return courier.NewChannel(id, kindOf(raw.Chat.Type), chatName(&raw.Chat))
}

// senderOf identifies who wrote a message. A message posted on behalf of a
// chat — an anonymous group administrator, a linked channel — has no user:
// the chat stands for it.
func senderOf(raw *apiMessage) courier.User {
	switch {
	case raw.SenderChat != nil:
		return courier.NewUser(userID(raw.SenderChat.ID), chatName(raw.SenderChat))
	case raw.From != nil:
		return courier.NewUser(userID(raw.From.ID), displayName(raw.From))
	default:
		return courier.NewUser(userID(raw.Chat.ID), chatName(&raw.Chat))
	}
}

func kindOf(chatType string) courier.ChannelKind {
	switch chatType {
	case "private":
		return courier.ChannelKindDirect
	case "group", "supergroup":
		return courier.ChannelKindGroup
	case "channel":
		return courier.ChannelKindPublic
	default:
		return courier.ChannelKindUnknown
	}
}

func chatName(chat *apiChat) string {
	if chat.Title != "" {
		return chat.Title
	}

	return displayName(&apiUser{FirstName: chat.FirstName, LastName: chat.LastName, Username: chat.Username})
}

func displayName(user *apiUser) string {
	name := strings.TrimSpace(user.FirstName + " " + user.LastName)
	if name == "" {
		name = user.Username
	}
	return name
}

func userID(id int64) courier.UserID {
	return courier.UserID(strconv.FormatInt(id, 10))
}

func chatChannelID(chatID int64) courier.ChannelID {
	return courier.ChannelID(strconv.FormatInt(chatID, 10))
}

func messageID(chatID int64, id int) courier.MessageID {
	return courier.MessageID(fmt.Sprintf("%d:%d", chatID, id))
}

// parseChannelID splits "<chat_id>[/<message_thread_id>]".
func parseChannelID(channelID courier.ChannelID) (int64, int, error) {
	chat, thread, hasThread := strings.Cut(string(channelID), "/")

	chatID, err := strconv.ParseInt(chat, 10, 64)
	if err != nil {
		return 0, 0, errors.Errorf("invalid telegram channel %q", channelID)
	}

	if !hasThread {
		return chatID, 0, nil
	}

	threadID, err := strconv.Atoi(thread)
	if err != nil {
		return 0, 0, errors.Errorf("invalid telegram channel %q", channelID)
	}

	return chatID, threadID, nil
}

// parseMessageID extracts the Telegram message identifier from
// "<chat_id>:<message_id>".
func parseMessageID(id courier.MessageID) (int, error) {
	raw := string(id)
	if idx := strings.LastIndexByte(raw, ':'); idx >= 0 {
		raw = raw[idx+1:]
	}

	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.Errorf("invalid telegram message id %q", id)
	}

	return value, nil
}

// splitText cuts text into chunks of at most limit UTF-16 code units, the
// unit Telegram counts in, preferring to cut after a new line, then after a
// space.
func splitText(text string, limit int) []string {
	if text == "" {
		return nil
	}

	var chunks []string

	for utf16Len(text) > limit {
		cut := cutIndex(text, limit)
		chunks = append(chunks, text[:cut])
		text = text[cut:]
	}

	if text != "" {
		chunks = append(chunks, text)
	}

	return chunks
}

// cutIndex returns the byte index where to cut text so that the head fits
// in limit UTF-16 code units.
func cutIndex(text string, limit int) int {
	var (
		units     int
		end       int
		lastLine  int
		lastSpace int
	)

	for idx, r := range text {
		size := utf16.RuneLen(r)
		if size < 0 {
			size = 1
		}
		// The first rune always goes, even past the limit: a cut at zero
		// would make splitText loop forever.
		if units+size > limit && end > 0 {
			break
		}
		units += size
		end = idx + len(string(r))

		switch r {
		case '\n':
			lastLine = end
		case ' ':
			lastSpace = end
		}
	}

	// Cutting too early would leave a tiny chunk: only fall back on a
	// boundary within the second half.
	switch {
	case lastLine > end/2:
		return lastLine
	case lastSpace > end/2:
		return lastSpace
	default:
		return end
	}
}

func utf16Len(text string) int {
	n := 0
	for _, r := range text {
		size := utf16.RuneLen(r)
		if size < 0 {
			size = 1
		}
		n += size
	}
	return n
}

func seconds(value int) time.Duration {
	return time.Duration(value) * time.Second
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

var (
	_ courier.Provider           = &Provider{}
	_ courier.ReactionProvider   = &Provider{}
	_ courier.StatusProvider     = &Provider{}
	_ courier.SelfProvider       = &Provider{}
	_ courier.ChannelResolver    = &Provider{}
	_ courier.CapabilityProvider = &Provider{}
)
