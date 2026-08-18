package rocket

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"time"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/syncx"
	"github.com/gopackage/ddp"
	"github.com/mitchellh/mapstructure"
	"github.com/pkg/errors"
)

type Update struct {
	EventName string `mapstructure:"eventName"`
	Args      []any  `mapstructure:"args"`
}

type MessageInfo struct {
	ID          string       `mapstructure:"_id"`
	RoomID      string       `mapstructure:"rid"`
	Message     string       `mapstructure:"msg"`
	ThreadID    string       `mapstructure:"tmid"`
	Timestamp   Timestamp    `mapstructure:"ts"`
	File        *FileInfo    `mapstructure:"file"`
	Files       []FileInfo   `mapstructure:"files"`
	Attachments []Attachment `mapstructure:"attachments"`
	Mentions    []UserInfo   `mapstructure:"mentions"`
	User        UserInfo     `mapstructure:"u"`
}

type UserInfo struct {
	ID       string `mapstructure:"_id"`
	Name     string `mapstructure:"name"`
	Username string `mapstructure:"username"`
}

// FileInfo describes a file uploaded alongside a message.
type FileInfo struct {
	ID   string `mapstructure:"_id"`
	Name string `mapstructure:"name"`
	Type string `mapstructure:"type"`
	Size int64  `mapstructure:"size"`
}

// Attachment holds the rendering metadata Rocket.Chat attaches to a message.
// The link to the actual file lives in TitleLink or in one of the media URLs.
type Attachment struct {
	Title       string `mapstructure:"title"`
	TitleLink   string `mapstructure:"title_link"`
	Description string `mapstructure:"description"`
	ImageURL    string `mapstructure:"image_url"`
	AudioURL    string `mapstructure:"audio_url"`
	VideoURL    string `mapstructure:"video_url"`
	ImageType   string `mapstructure:"image_type"`
	AudioType   string `mapstructure:"audio_type"`
	VideoType   string `mapstructure:"video_type"`
}

// link returns the server relative path of the attached file, if any.
func (a Attachment) link() string {
	for _, candidate := range []string{a.TitleLink, a.ImageURL, a.AudioURL, a.VideoURL} {
		if candidate != "" {
			return candidate
		}
	}

	return ""
}

// contentType returns the declared content type of the attached file.
func (a Attachment) contentType() string {
	for _, candidate := range []string{a.ImageType, a.AudioType, a.VideoType} {
		if candidate != "" {
			return candidate
		}
	}

	return ""
}

// Timestamp decodes the Rocket.Chat date representation, sent over DDP as
// {"$date": <millis>}.
type Timestamp struct {
	Date int64 `mapstructure:"$date"`
}

func (t Timestamp) Time() time.Time {
	if t.Date == 0 {
		return time.Now()
	}

	return time.UnixMilli(t.Date)
}

type RoomInfo struct {
	IsParticipant bool   `mapstructure:"roomParticipant"`
	Type          string `mapstructure:"roomType"`
	Name          string `mapstructure:"roomName"`
}

type messageListener struct {
	ctx              context.Context
	provider         *Provider
	username         string
	messageChan      chan courier.Message
	receivedMessages syncx.Map[string, struct{}]
}

// CollectionUpdate implements ddp.UpdateListener.
//
// Every room the account takes part in is forwarded, group rooms included.
// Deciding whether to answer is the application's call.
func (l *messageListener) CollectionUpdate(collection string, operation string, id string, doc ddp.Update) {
	update := Update{}
	if err := mapstructure.Decode(doc, &update); err != nil {
		slog.Error("could not decode ddp update", slog.Any("error", errors.WithStack(err)))
		return
	}

	if len(update.Args) != 2 || update.EventName != "__my_messages__" {
		return
	}

	roomInfo := RoomInfo{}

	if err := mapstructure.Decode(update.Args[1], &roomInfo); err != nil {
		slog.Error("could not decode room info", slog.Any("error", errors.WithStack(err)))
		return
	}

	messageInfo := MessageInfo{}

	if err := mapstructure.Decode(update.Args[0], &messageInfo); err != nil {
		slog.Error("could not decode message info", slog.Any("error", errors.WithStack(err)))
		return
	}

	if messageInfo.User.Username == l.username {
		return
	}

	if _, exists := l.receivedMessages.LoadOrStore(messageInfo.ID, struct{}{}); exists {
		return
	}

	message := l.toMessage(messageInfo, roomInfo)

	select {
	case l.messageChan <- message:
	case <-l.ctx.Done():
	}
}

func (l *messageListener) toMessage(messageInfo MessageInfo, roomInfo RoomInfo) courier.Message {
	funcs := []courier.BaseMessageOptionFunc{
		courier.WithMessageMainPart(messageInfo.Message),
		courier.WithMessageSentAt(messageInfo.Timestamp.Time()),
	}

	for _, attachment := range l.toAttachments(messageInfo) {
		funcs = append(funcs, courier.WithMessagePart(attachment))
	}

	if mentions := toMentions(messageInfo.Mentions); len(mentions) > 0 {
		funcs = append(funcs, courier.WithMessageMentions(mentions...))
	}

	if messageInfo.ThreadID != "" {
		funcs = append(funcs, courier.WithMessageInReplyTo(courier.MessageID(messageInfo.ThreadID)))
	}

	return courier.NewMessage(
		courier.MessageID(messageInfo.ID),
		courier.NewChannel(courier.ChannelID(messageInfo.RoomID), kindOf(roomInfo.Type), roomInfo.Name),
		courier.NewUser(courier.UserID(messageInfo.User.ID), displayNameOf(messageInfo.User)),
		funcs...,
	)
}

// toAttachments pairs the file descriptors with the rendering metadata
// holding the download link. Both arrive in the same message, in the same
// order.
func (l *messageListener) toAttachments(messageInfo MessageInfo) []courier.Attachment {
	files := messageInfo.Files
	if len(files) == 0 && messageInfo.File != nil {
		files = []FileInfo{*messageInfo.File}
	}

	attachments := make([]courier.Attachment, 0, len(messageInfo.Attachments))

	for idx, metadata := range messageInfo.Attachments {
		link := metadata.link()
		if link == "" {
			continue
		}

		filename := metadata.Title
		contentType := metadata.contentType()
		size := int64(-1)

		if idx < len(files) {
			file := files[idx]

			if file.Name != "" {
				filename = file.Name
			}

			if file.Type != "" {
				contentType = file.Type
			}

			size = file.Size
		}

		if contentType == "" {
			contentType = "application/octet-stream"
		}

		attachments = append(attachments, l.provider.toAttachment(
			idx, filename, contentType, size, link, metadata.Description,
		))
	}

	return attachments
}

// toAttachment wraps a Rocket.Chat upload. The content is only downloaded on
// first read, through the authenticated REST API.
func (p *Provider) toAttachment(index int, filename, contentType string, size int64, link, caption string) courier.Attachment {
	download := func(ctx context.Context) (io.ReadCloser, error) {
		reader, err := p.rest.download(ctx, link)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		return reader, nil
	}

	open, release := courier.BufferedOpener(download, p.opts.MaxInMemorySize)
	p.trackRelease(release)

	funcs := []courier.AttachmentOptionFunc{
		courier.WithAttachmentName("att-" + strconv.Itoa(index)),
		courier.WithAttachmentCaption(caption),
	}

	if size >= 0 {
		funcs = append(funcs, courier.WithAttachmentSize(size))
	}

	return courier.NewAttachment(filename, contentType, open, funcs...)
}

// kindOf classifies a Rocket.Chat room type.
func kindOf(roomType string) courier.ChannelKind {
	switch roomType {
	case "d":
		return courier.ChannelKindDirect
	case "p":
		return courier.ChannelKindGroup
	case "c", "l":
		return courier.ChannelKindPublic
	default:
		return courier.ChannelKindUnknown
	}
}

func toMentions(users []UserInfo) []courier.Mention {
	mentions := make([]courier.Mention, 0, len(users))

	for _, user := range users {
		mentions = append(mentions, courier.Mention{
			UserID:      courier.UserID(user.ID),
			DisplayName: displayNameOf(user),
		})
	}

	return mentions
}

func displayNameOf(user UserInfo) string {
	if user.Name != "" {
		return user.Name
	}

	return user.Username
}

var _ ddp.UpdateListener = &messageListener{}
