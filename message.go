package courier

import (
	"context"
	"io"
	"time"

	"github.com/pkg/errors"
	"github.com/rs/xid"
)

const (
	PartMain  string = "_main"
	TextPlain string = "text/plain"
)

type MessageID string

func RandomMessageID() MessageID {
	return MessageID(xid.New().String())
}

type Message interface {
	ID() MessageID
	From() User
	SentAt() time.Time
	Parts() []MessagePart
	Channel() Channel
}

// MessagePart is one piece of a message content. A message always carries a
// main part, named PartMain, plus any number of additional parts such as
// attachments.
//
// Reader takes a context because opening a part may hit the network: media
// hosted by the messaging platform is only downloaded when the part is
// actually read.
type MessagePart interface {
	Name() string
	ContentType() string
	Reader(ctx context.Context) (io.ReadCloser, error)
}

// MentionedMessage is implemented by messages carrying explicit mentions of
// other users. Use the Mentions helper rather than asserting directly.
type MentionedMessage interface {
	Message
	Mentions() []Mention
}

// ThreadedMessage is implemented by messages replying to another one. Use the
// InReplyTo helper rather than asserting directly.
type ThreadedMessage interface {
	Message
	InReplyTo() MessageID
}

// Mention references a user explicitly named in a message.
type Mention struct {
	UserID      UserID
	DisplayName string
}

type BaseMessage struct {
	id           MessageID
	channel      Channel
	from         User
	parts        []MessagePart
	sentAt       time.Time
	mentions     []Mention
	inReplyTo    MessageID
	linkPreviews []LinkPreview
}

// Channel implements Message.
func (m *BaseMessage) Channel() Channel {
	return m.channel
}

// From implements Message.
func (m *BaseMessage) From() User {
	return m.from
}

// ID implements Message.
func (m *BaseMessage) ID() MessageID {
	return m.id
}

// Parts implements Message.
func (m *BaseMessage) Parts() []MessagePart {
	return m.parts
}

// SentAt implements Message.
func (m *BaseMessage) SentAt() time.Time {
	return m.sentAt
}

// Mentions implements MentionedMessage.
func (m *BaseMessage) Mentions() []Mention {
	return m.mentions
}

// InReplyTo implements ThreadedMessage.
func (m *BaseMessage) InReplyTo() MessageID {
	return m.inReplyTo
}

// LinkPreviews implements LinkPreviewMessage.
func (m *BaseMessage) LinkPreviews() []LinkPreview {
	return m.linkPreviews
}

type BaseMessageOptions struct {
	SentAt       time.Time
	Parts        []MessagePart
	Mentions     []Mention
	InReplyTo    MessageID
	LinkPreviews []LinkPreview
}

type BaseMessageOptionFunc func(opts *BaseMessageOptions)

func NewBaseMessageOptions(funcs ...BaseMessageOptionFunc) *BaseMessageOptions {
	opts := &BaseMessageOptions{
		SentAt:   time.Now(),
		Parts:    []MessagePart{},
		Mentions: []Mention{},
	}
	for _, fn := range funcs {
		fn(opts)
	}
	return opts
}

// WithMessageMainPart sets the main, textual content of the message.
func WithMessageMainPart(text string) BaseMessageOptionFunc {
	return WithMessageMainPartOfType(text, TextPlain)
}

// WithMessageMainPartOfType sets the main content of the message with an
// explicit content type, for providers able to carry markup such as HTML or
// Markdown.
func WithMessageMainPartOfType(text string, contentType string) BaseMessageOptionFunc {
	return func(opts *BaseMessageOptions) {
		opts.Parts = append(opts.Parts, NewMessagePart(PartMain, contentType, OpenerFromString(text)))
	}
}

func WithMessagePart(part MessagePart) BaseMessageOptionFunc {
	return func(opts *BaseMessageOptions) {
		opts.Parts = append(opts.Parts, part)
	}
}

func WithMessageSentAt(sentAt time.Time) BaseMessageOptionFunc {
	return func(opts *BaseMessageOptions) {
		opts.SentAt = sentAt
	}
}

// WithMessageMentions declares the users explicitly named in the message.
func WithMessageMentions(mentions ...Mention) BaseMessageOptionFunc {
	return func(opts *BaseMessageOptions) {
		opts.Mentions = append(opts.Mentions, mentions...)
	}
}

// WithMessageInReplyTo declares the message this one replies to.
func WithMessageInReplyTo(messageID MessageID) BaseMessageOptionFunc {
	return func(opts *BaseMessageOptions) {
		opts.InReplyTo = messageID
	}
}

// WithMessageLinkPreviews declares the link preview cards carried by the
// message.
func WithMessageLinkPreviews(previews ...LinkPreview) BaseMessageOptionFunc {
	return func(opts *BaseMessageOptions) {
		opts.LinkPreviews = append(opts.LinkPreviews, previews...)
	}
}

func NewMessage(id MessageID, channel Channel, from User, funcs ...BaseMessageOptionFunc) *BaseMessage {
	opts := NewBaseMessageOptions(funcs...)
	return &BaseMessage{
		id:           id,
		channel:      channel,
		from:         from,
		parts:        opts.Parts,
		sentAt:       opts.SentAt,
		mentions:     opts.Mentions,
		inReplyTo:    opts.InReplyTo,
		linkPreviews: opts.LinkPreviews,
	}
}

var (
	_ Message            = &BaseMessage{}
	_ MentionedMessage   = &BaseMessage{}
	_ ThreadedMessage    = &BaseMessage{}
	_ LinkPreviewMessage = &BaseMessage{}
)

type BaseMessagePart struct {
	name        string
	contentType string
	open        PartOpener
}

// Name implements MessagePart.
func (p *BaseMessagePart) Name() string {
	return p.name
}

// ContentType implements MessagePart.
func (p *BaseMessagePart) ContentType() string {
	return p.contentType
}

// Reader implements MessagePart.
func (p *BaseMessagePart) Reader(ctx context.Context) (io.ReadCloser, error) {
	if p.open == nil {
		return nil, errors.WithStack(ErrNotFound)
	}

	reader, err := p.open(ctx)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return reader, nil
}

func NewMessagePart(name string, contentType string, open PartOpener) *BaseMessagePart {
	return &BaseMessagePart{
		name:        name,
		contentType: contentType,
		open:        open,
	}
}

var _ MessagePart = &BaseMessagePart{}

// Mentions returns the users explicitly named in the message, or nil when the
// provider does not support mentions.
func Mentions(message Message) []Mention {
	mentioned, ok := message.(MentionedMessage)
	if !ok {
		return nil
	}

	return mentioned.Mentions()
}

// IsMentioned reports whether the given user is explicitly named in the
// message.
func IsMentioned(message Message, userID UserID) bool {
	for _, m := range Mentions(message) {
		if m.UserID == userID {
			return true
		}
	}

	return false
}

// InReplyTo returns the message this one replies to, if any.
func InReplyTo(message Message) (MessageID, bool) {
	threaded, ok := message.(ThreadedMessage)
	if !ok {
		return "", false
	}

	parent := threaded.InReplyTo()
	if parent == "" {
		return "", false
	}

	return parent, true
}

func GetMessageMainPart(message Message) (MessagePart, error) {
	for _, p := range message.Parts() {
		if p.Name() != PartMain {
			continue
		}

		return p, nil
	}

	return nil, errors.WithStack(ErrNotFound)
}

func GetMessageMainContent(ctx context.Context, message Message) (string, error) {
	main, err := GetMessageMainPart(message)
	if err != nil {
		return "", errors.WithStack(err)
	}

	data, err := ReadPart(ctx, main)
	if err != nil {
		return "", errors.WithStack(err)
	}

	return string(data), nil
}

func GetMessageContentByType(ctx context.Context, message Message, contentType string) ([]byte, error) {
	for _, p := range message.Parts() {
		if p.ContentType() != contentType {
			continue
		}

		data, err := ReadPart(ctx, p)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		return data, nil
	}

	return nil, errors.WithStack(ErrNotFound)
}

// ReadPart reads a part content entirely, closing the underlying reader.
func ReadPart(ctx context.Context, part MessagePart) ([]byte, error) {
	reader, err := part.Reader(ctx)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return data, nil
}
