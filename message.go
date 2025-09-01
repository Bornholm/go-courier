package courier

import (
	"bytes"
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
	ChannelID() ChannelID
}

type MessagePart interface {
	Name() string
	ContentType() string
	Reader() (io.ReadCloser, error)
}

type BaseMessage struct {
	id        MessageID
	channelID ChannelID
	from      User
	parts     []MessagePart
	sentAt    time.Time
}

// ChannelID implements Message.
func (m *BaseMessage) ChannelID() ChannelID {
	return m.channelID
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

type BaseMessageOptions struct {
	SentAt time.Time
	Parts  []MessagePart
}

type BaseMessageOptionFunc func(opts *BaseMessageOptions)

func NewBaseMessageOptions(funcs ...BaseMessageOptionFunc) *BaseMessageOptions {
	opts := &BaseMessageOptions{
		SentAt: time.Now(),
		Parts:  []MessagePart{},
	}
	for _, fn := range funcs {
		fn(opts)
	}
	return opts
}

type mainPart string

// ContentType implements MessagePart.
func (t mainPart) ContentType() string {
	return TextPlain
}

// Name implements MessagePart.
func (t mainPart) Name() string {
	return PartMain
}

// Reader implements MessagePart.
func (t mainPart) Reader() (io.ReadCloser, error) {
	reader := io.NopCloser(bytes.NewBufferString(string(t)))
	return reader, nil
}

var _ MessagePart = mainPart("")

func WithMessageMainPart(text string) BaseMessageOptionFunc {
	return func(opts *BaseMessageOptions) {
		opts.Parts = append(opts.Parts, mainPart(text))
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

func NewMessage(id MessageID, channelID ChannelID, from User, funcs ...BaseMessageOptionFunc) *BaseMessage {
	opts := NewBaseMessageOptions(funcs...)
	return &BaseMessage{
		id:        id,
		channelID: channelID,
		from:      from,
		parts:     opts.Parts,
		sentAt:    opts.SentAt,
	}
}

var _ Message = &BaseMessage{}

type BaseMessagePart struct {
	name        string
	contentType string
	reader      io.ReadCloser
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
func (p *BaseMessagePart) Reader() (io.ReadCloser, error) {
	return p.reader, nil
}

func NewMessagePart(name string, contentType string, reader io.ReadCloser) *BaseMessagePart {
	return &BaseMessagePart{
		name:        name,
		contentType: contentType,
		reader:      reader,
	}
}

var _ MessagePart = &BaseMessagePart{}

func GetMessageMainPart(message Message) (MessagePart, error) {
	for _, p := range message.Parts() {
		if p.Name() != PartMain {
			continue
		}

		return p, nil
	}

	return nil, errors.WithStack(ErrNotFound)
}

func GetMessageMainContent(message Message) (string, error) {
	main, err := GetMessageMainPart(message)
	if err != nil {
		return "", errors.WithStack(err)
	}

	reader, err := main.Reader()
	if err != nil {
		return "", errors.WithStack(err)
	}

	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return "", errors.WithStack(err)
	}

	return string(data), nil
}

func GetMessageContentByType(message Message, contentType string) ([]byte, error) {
	for _, p := range message.Parts() {
		if p.ContentType() != contentType {
			continue
		}

		reader, err := p.Reader()
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

	return nil, errors.WithStack(ErrNotFound)
}
