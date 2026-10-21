// Package whatsappcloud implements a courier.Provider over the WhatsApp
// Business Platform Cloud API, the official interface Meta offers to
// businesses.
//
// Unlike provider/whatsapp, which links itself to an ordinary account as a
// companion device, this provider talks to a dedicated business number
// through the Graph API. Messages are sent with HTTPS calls and received
// through a webhook: the provider is an http.Handler the application mounts
// on its own server, at the URL declared in the Meta application.
//
// The rules differ from an ordinary account in ways an application must
// know about:
//
//   - Free-form messages only reach users who wrote in the last 24 hours.
//     Past that, see ReengageFunc and window.go.
//   - Users are identified by their business-scoped user ID (BSUID, "FR.…")
//     when WhatsApp reports one, by their phone number otherwise.
//   - Groups are capped at 8 participants and can only be joined through an
//     invite link. Their channel identifiers carry the "group:" prefix.
package whatsappcloud

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/syncx"
	"github.com/pkg/errors"
)

// groupPrefix marks the channel identifiers of groups, so that Send knows
// how to address them without asking the platform.
const groupPrefix = "group:"

// eventBufferSize is the number of webhook notifications queued before the
// endpoint starts answering 503, which makes Meta deliver them again later.
const eventBufferSize = 64

// inflightTTL is how long a sent message is remembered, in case WhatsApp
// reports afterwards that it was refused for a closed window.
const inflightTTL = time.Hour

// seenTTL is how long a received message identifier is remembered, to
// ignore the notifications Meta delivers twice.
const seenTTL = 24 * time.Hour

type Provider struct {
	opts *Options

	mu     sync.Mutex
	events chan webhookPayload

	reactMu   sync.Mutex
	reactions chan courier.Reaction

	selfMu sync.Mutex
	self   courier.User

	groupNames syncx.Map[string, string]

	// lastInbound remembers the last message received on each channel:
	// the typing indicator is attached to a message, not to a chat.
	lastInbound syncx.Map[courier.ChannelID, string]

	seenMu sync.Mutex
	seen   map[string]time.Time

	inflightMu sync.Mutex
	inflight   map[string]sentMessage

	outbox outbox

	releaseMu sync.Mutex
	release   []courier.CloseFunc

	now func() time.Time
}

type sentMessage struct {
	message courier.Message
	sentAt  time.Time
}

func NewProvider(funcs ...OptionFunc) *Provider {
	return &Provider{
		opts:     NewOptions(funcs...),
		seen:     map[string]time.Time{},
		inflight: map[string]sentMessage{},
		now:      time.Now,
	}
}

// Listen implements courier.Provider. Messages reach the returned channel
// once the webhook, served by ServeHTTP, receives them.
func (p *Provider) Listen(ctx context.Context) (chan courier.Message, error) {
	p.mu.Lock()
	if p.events != nil {
		p.mu.Unlock()
		return nil, errors.New("provider is already listening")
	}
	events := make(chan webhookPayload, eventBufferSize)
	p.events = events
	p.mu.Unlock()

	messages := make(chan courier.Message)

	go func() {
		defer close(messages)
		defer p.releaseAll()
		defer func() {
			p.mu.Lock()
			p.events = nil
			p.mu.Unlock()
		}()

		for {
			select {
			case <-ctx.Done():
				return
			case payload := <-events:
				p.process(ctx, payload, messages)
			}
		}
	}()

	return messages, nil
}

// enqueue hands a notification to the listening goroutine without ever
// blocking the webhook.
func (p *Provider) enqueue(payload webhookPayload) bool {
	p.mu.Lock()
	events := p.events
	p.mu.Unlock()

	if events == nil {
		return false
	}

	select {
	case events <- payload:
		return true
	default:
		return false
	}
}

// ListenReactions implements courier.ReactionProvider. Reactions arrive
// through the webhook like messages do, hence only while Listen runs.
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

// process handles one webhook notification. It runs on the listening
// goroutine, one notification at a time, which keeps the order of a
// conversation.
func (p *Provider) process(ctx context.Context, payload webhookPayload, messages chan courier.Message) {
	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			if change.Field != "messages" {
				continue
			}

			value := change.Value

			// One Meta application may serve several numbers: only ours
			// concern this provider.
			if value.Metadata.PhoneNumberID != "" && value.Metadata.PhoneNumberID != p.opts.PhoneNumberID {
				continue
			}

			for _, status := range value.Statuses {
				p.handleStatus(ctx, status)
			}

			names := contactNames(value.Contacts)

			for _, raw := range value.Messages {
				if !p.firstSeen(raw.ID) {
					continue
				}

				p.handleMessage(ctx, raw, names, messages)
			}
		}
	}
}

func (p *Provider) handleMessage(ctx context.Context, raw webhookMessage, names map[string]string, messages chan courier.Message) {
	from := userOf(raw, names)
	channel := p.channelFor(ctx, raw, from)
	sentAt := timestampOf(raw.Timestamp, p.now())

	if raw.Type == "reaction" && raw.Reaction != nil {
		p.emitReaction(ctx, courier.Reaction{
			MessageID: courier.MessageID(raw.Reaction.MessageID),
			Channel:   channel,
			From:      from,
			Emoji:     raw.Reaction.Emoji,
			ReactedAt: sentAt,
		})
		return
	}

	message := p.toMessage(ctx, raw, channel, from, sentAt)
	if message == nil {
		slog.DebugContext(ctx, "ignoring whatsapp cloud message", slog.String("type", raw.Type))
		return
	}

	p.lastInbound.Store(channel.ChannelID(), raw.ID)

	if channel.Kind() == courier.ChannelKindDirect {
		if err := p.opts.Windows.RecordInbound(ctx, from.ID(), sentAt); err != nil {
			slog.WarnContext(ctx, "could not record whatsapp service window", slog.Any("error", errors.WithStack(err)))
		}

		// The user wrote: the window is open again, deliver what was held
		// back before the application answers this message.
		p.flush(ctx, channel.ChannelID())
	}

	select {
	case messages <- message:
	case <-ctx.Done():
	}
}

func (p *Provider) emitReaction(ctx context.Context, reaction courier.Reaction) {
	p.reactMu.Lock()
	defer p.reactMu.Unlock()

	if p.reactions == nil {
		return
	}

	// Never wait for a consumer that stopped reading: the notifications
	// of every conversation would queue up behind it. A reaction is only
	// feedback, losing one is the lesser harm.
	select {
	case p.reactions <- reaction:
	default:
		slog.WarnContext(ctx, "whatsapp reaction dropped, nobody reads them")
	}
}

func (p *Provider) handleStatus(ctx context.Context, status webhookStatus) {
	if status.Status != "failed" {
		if status.Status == "delivered" || status.Status == "read" {
			p.inflightMu.Lock()
			delete(p.inflight, status.ID)
			p.inflightMu.Unlock()
		}
		return
	}

	for _, statusErr := range status.Errors {
		if statusErr.Code == codeReengagement {
			p.inflightMu.Lock()
			sent, found := p.inflight[status.ID]
			delete(p.inflight, status.ID)
			p.inflightMu.Unlock()

			if !found {
				slog.WarnContext(ctx, "whatsapp refused a message for a closed service window, too late to hold it back")
				return
			}

			if _, err := p.holdBack(ctx, sent.message); err != nil {
				slog.WarnContext(ctx, "could not hold back a message refused for a closed service window", slog.Any("error", err))
			}
			return
		}

		slog.WarnContext(ctx, "whatsapp could not deliver a message",
			slog.Int("code", statusErr.Code),
			slog.String("title", statusErr.Title),
		)
	}
}

// firstSeen reports whether the message identifier was never received
// before: Meta delivers a notification again when it doubts it was
// acknowledged.
func (p *Provider) firstSeen(id string) bool {
	if id == "" {
		return true
	}

	now := p.now()

	p.seenMu.Lock()
	defer p.seenMu.Unlock()

	if _, found := p.seen[id]; found {
		return false
	}

	if len(p.seen) >= 4096 {
		for seenID, at := range p.seen {
			if now.Sub(at) > seenTTL {
				delete(p.seen, seenID)
			}
		}
	}

	p.seen[id] = now

	return true
}

func (p *Provider) trackInflight(id courier.MessageID, message courier.Message) {
	now := p.now()

	p.inflightMu.Lock()
	defer p.inflightMu.Unlock()

	for sentID, sent := range p.inflight {
		if now.Sub(sent.sentAt) > inflightTTL {
			delete(p.inflight, sentID)
		}
	}

	p.inflight[string(id)] = sentMessage{message: message, sentAt: now}
}

// Send implements courier.Provider.
//
// To a user whose service window is closed, the message is held back and a
// template sent instead, see window.go. The identifier returned is then the
// template's.
func (p *Provider) Send(ctx context.Context, message courier.Message) (courier.MessageID, error) {
	channel := message.Channel()
	if channel == nil {
		return "", errors.New("message has no channel")
	}

	channelID := channel.ChannelID()

	if isGroup(channelID) {
		id, err := p.deliver(ctx, message)
		return id, errors.WithStack(err)
	}

	last, err := p.opts.Windows.LastInbound(ctx, courier.UserID(channelID))
	if err != nil {
		slog.WarnContext(ctx, "could not read whatsapp service window", slog.Any("error", errors.WithStack(err)))
	}

	if !last.IsZero() && p.now().Sub(last) > p.opts.ServiceWindow {
		return p.holdBack(ctx, message)
	}

	id, err := p.deliver(ctx, message)
	if IsWindowClosed(err) {
		return p.holdBack(ctx, message)
	}
	if err != nil {
		return "", errors.WithStack(err)
	}

	p.trackInflight(id, message)

	return id, nil
}

// holdBack keeps message until the recipient writes again, and sends them a
// template when nothing was held back for them yet.
func (p *Provider) holdBack(ctx context.Context, message courier.Message) (courier.MessageID, error) {
	if p.opts.Reengage == nil {
		return "", errors.WithStack(ErrWindowClosed)
	}

	recipient := message.Channel().ChannelID()

	content, err := courier.GetMessageMainContent(ctx, message)
	if err != nil && !errors.Is(err, courier.ErrNotFound) {
		return "", errors.WithStack(err)
	}

	template, ok := p.opts.Reengage(ctx, recipient, previewOf(content))
	if !ok {
		return "", errors.WithStack(ErrWindowClosed)
	}

	first, dropped := p.outbox.push(recipient, message, p.now(), p.opts.OutboxTTL, p.opts.OutboxSize)
	if dropped > 0 {
		slog.WarnContext(ctx, "whatsapp outbox full, oldest held back messages dropped", slog.Int("dropped", dropped))
	}

	if !first {
		// A template already invites the user to write back: everything
		// held back will be delivered at once when they do.
		return message.ID(), nil
	}

	id, err := p.sendTemplate(ctx, recipient, template)
	if err != nil {
		// Nothing told the user anything is waiting: holding the message
		// back would only hide the failure.
		p.outbox.take(recipient, p.now(), p.opts.OutboxTTL)
		return "", errors.Wrap(err, "could not send the re-engagement template")
	}

	return id, nil
}

// flush delivers what was held back for recipient.
func (p *Provider) flush(ctx context.Context, recipient courier.ChannelID) {
	for _, message := range p.outbox.take(recipient, p.now(), p.opts.OutboxTTL) {
		id, err := p.deliver(ctx, message)
		if err != nil {
			slog.WarnContext(ctx, "could not deliver a held back whatsapp message", slog.Any("error", err))
			continue
		}

		p.trackInflight(id, message)
	}
}

func (p *Provider) sendTemplate(ctx context.Context, recipient courier.ChannelID, template Template) (courier.MessageID, error) {
	payload := addressed(recipient)
	payload.Type = "template"
	payload.Template = &outgoingTmpl{
		Name:     template.Name,
		Language: outgoingTmplLanguage{Code: template.Language},
	}

	if len(template.BodyParameters) > 0 {
		params := make([]outgoingTmplParam, 0, len(template.BodyParameters))
		for _, value := range template.BodyParameters {
			params = append(params, outgoingTmplParam{Type: "text", Text: value})
		}
		payload.Template.Components = []outgoingTmplComp{{Type: "body", Parameters: params}}
	}

	id, err := p.postMessage(ctx, payload)
	if err != nil {
		return "", errors.WithStack(err)
	}

	return courier.MessageID(id), nil
}

// SetStatus implements courier.StatusProvider. The typing indicator of the
// Cloud API hangs on the last received message, which it also marks as
// read; it fades by itself after 25 seconds or when the answer is sent.
func (p *Provider) SetStatus(ctx context.Context, status courier.Status, channelID courier.ChannelID) error {
	if status != courier.StatusTyping {
		return nil
	}

	messageID, found := p.lastInbound.Load(channelID)
	if !found {
		return nil
	}

	_, err := p.postStatus(ctx, outgoing{
		Status:          "read",
		MessageID:       messageID,
		TypingIndicator: &typingIndicator{Type: "text"},
	})

	return errors.WithStack(err)
}

func (p *Provider) postStatus(ctx context.Context, payload outgoing) (bool, error) {
	payload.MessagingProduct = "whatsapp"

	var result struct {
		Success bool `json:"success"`
	}
	if err := p.callJSON(ctx, http.MethodPost, p.opts.PhoneNumberID+"/messages", payload, &result); err != nil {
		return false, errors.WithStack(err)
	}

	return result.Success, nil
}

// Self implements courier.SelfProvider. The business number, digits only,
// is the identity: it is what group members type to mention it.
func (p *Provider) Self(ctx context.Context) (courier.User, error) {
	p.selfMu.Lock()
	defer p.selfMu.Unlock()

	if p.self != nil {
		return p.self, nil
	}

	var number struct {
		DisplayPhoneNumber string `json:"display_phone_number"`
		VerifiedName       string `json:"verified_name"`
	}

	path := p.opts.PhoneNumberID + "?fields=" + url.QueryEscape("display_phone_number,verified_name")
	if err := p.callJSON(ctx, http.MethodGet, path, nil, &number); err != nil {
		return nil, errors.WithStack(err)
	}

	p.self = courier.NewUser(courier.UserID(digitsOf(number.DisplayPhoneNumber)), number.VerifiedName)

	return p.self, nil
}

// Channel implements courier.ChannelResolver.
func (p *Provider) Channel(ctx context.Context, channelID courier.ChannelID) (courier.Channel, error) {
	if groupID, ok := strings.CutPrefix(string(channelID), groupPrefix); ok {
		return courier.NewChannel(channelID, courier.ChannelKindGroup, p.groupName(ctx, groupID)), nil
	}

	return courier.NewChannel(channelID, courier.ChannelKindDirect, ""), nil
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

func (p *Provider) channelFor(ctx context.Context, raw webhookMessage, from courier.User) courier.Channel {
	if raw.GroupID != "" {
		return courier.NewChannel(courier.ChannelID(groupPrefix+raw.GroupID), courier.ChannelKindGroup, p.groupName(ctx, raw.GroupID))
	}

	return courier.NewChannel(courier.ChannelID(from.ID()), courier.ChannelKindDirect, from.DisplayName())
}

// groupName resolves the subject of a group, once: it is a network call.
func (p *Provider) groupName(ctx context.Context, groupID string) string {
	if name, found := p.groupNames.Load(groupID); found {
		return name
	}

	var group struct {
		Subject string `json:"subject"`
	}
	if err := p.callJSON(ctx, http.MethodGet, url.PathEscape(groupID)+"?fields=subject", nil, &group); err != nil {
		slog.DebugContext(ctx, "could not resolve whatsapp group subject", slog.Any("error", err))
		return ""
	}

	p.groupNames.Store(groupID, group.Subject)

	return group.Subject
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
			slog.Error("could not release media content", slog.Any("error", errors.WithStack(err)))
		}
	}
}

// addressed starts a message payload to channelID: a group, a
// business-scoped user ID or a phone number, each with its own field.
func addressed(channelID courier.ChannelID) outgoing {
	id := string(channelID)

	if groupID, ok := strings.CutPrefix(id, groupPrefix); ok {
		return outgoing{RecipientType: "group", To: groupID}
	}

	if strings.Contains(id, ".") {
		return outgoing{RecipientType: "individual", Recipient: id}
	}

	return outgoing{RecipientType: "individual", To: id}
}

func isGroup(channelID courier.ChannelID) bool {
	return strings.HasPrefix(string(channelID), groupPrefix)
}

// userOf identifies the sender: by business-scoped user ID when WhatsApp
// reports one, since a user who enabled a username may hide their phone
// number, by phone number otherwise.
func userOf(raw webhookMessage, names map[string]string) courier.User {
	id := raw.FromUserID
	if id == "" {
		id = raw.From
	}

	name := names[id]
	if name == "" {
		name = names[raw.From]
	}

	return courier.NewUser(courier.UserID(id), name)
}

func contactNames(contacts []webhookContact) map[string]string {
	names := make(map[string]string, len(contacts)*2)
	for _, contact := range contacts {
		if contact.UserID != "" {
			names[contact.UserID] = contact.Profile.Name
		}
		if contact.WaID != "" {
			names[contact.WaID] = contact.Profile.Name
		}
	}
	return names
}

func timestampOf(raw string, fallback time.Time) time.Time {
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seconds <= 0 {
		return fallback
	}
	return time.Unix(seconds, 0)
}

// mentionPattern finds the "@…" tokens of a text. In WhatsApp, a mention is
// written in the text itself, as the phone number of the person mentioned.
var mentionPattern = regexp.MustCompile(`(?:^|[\s(])@([\p{L}\p{N}._-]+)`)

func mentionsOf(text string, self courier.User) []courier.Mention {
	var mentions []courier.Mention

	for _, match := range mentionPattern.FindAllStringSubmatch(text, -1) {
		token := strings.TrimRight(match[1], "._-")
		if token == "" {
			continue
		}

		id := courier.UserID(token)

		// The business name is accepted as well, for whoever types it
		// instead of picking the number from the suggestions.
		if self != nil && self.DisplayName() != "" && strings.EqualFold(token, strings.ReplaceAll(self.DisplayName(), " ", "")) {
			id = self.ID()
		}

		mentions = append(mentions, courier.Mention{UserID: id, DisplayName: token})
	}

	return mentions
}

func digitsOf(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func describeLocation(name, address string, latitude, longitude float64) string {
	parts := []string{}
	if name != "" {
		parts = append(parts, name)
	}
	if address != "" {
		parts = append(parts, address)
	}
	parts = append(parts, fmt.Sprintf("https://maps.google.com/?q=%f,%f", latitude, longitude))
	return "📍 " + strings.Join(parts, ", ")
}

var (
	_ courier.Provider           = &Provider{}
	_ courier.ReactionProvider   = &Provider{}
	_ courier.StatusProvider     = &Provider{}
	_ courier.SelfProvider       = &Provider{}
	_ courier.ChannelResolver    = &Provider{}
	_ courier.CapabilityProvider = &Provider{}
	_ http.Handler               = &Provider{}
)
