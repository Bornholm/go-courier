package rest

import (
	"context"
	"fmt"
	"time"

	"github.com/bornholm/go-courier"
	"github.com/pkg/errors"
)

// MessageDTO is the JSON representation of a message.
type MessageDTO struct {
	ID        string       `json:"id"`
	Channel   ChannelDTO   `json:"channel"`
	From      UserDTO      `json:"from"`
	SentAt    time.Time    `json:"sentAt"`
	InReplyTo string       `json:"inReplyTo,omitempty"`
	Mentions  []MentionDTO `json:"mentions,omitempty"`
	Parts     []PartDTO    `json:"parts"`
}

type ChannelDTO struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`
}

type UserDTO struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName,omitempty"`
}

type MentionDTO struct {
	UserID      string `json:"userId"`
	DisplayName string `json:"displayName,omitempty"`
}

// PartDTO describes a message part. Textual parts below the configured limit
// carry their content inline; every other part is only reachable through URL.
type PartDTO struct {
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	Kind        string `json:"kind"`
	Content     string `json:"content,omitempty"`
	Filename    string `json:"filename,omitempty"`
	Size        int64  `json:"size,omitempty"`
	Disposition string `json:"disposition,omitempty"`
	Caption     string `json:"caption,omitempty"`
	VoiceNote   bool   `json:"voiceNote,omitempty"`
	DurationMs  int64  `json:"durationMs,omitempty"`
	URL         string `json:"url,omitempty"`
}

// IncomingMessageDTO is the JSON body of the "message" field of an incoming
// multipart request.
type IncomingMessageDTO struct {
	Content     string       `json:"content"`
	ContentType string       `json:"contentType,omitempty"`
	InReplyTo   string       `json:"inReplyTo,omitempty"`
	Mentions    []MentionDTO `json:"mentions,omitempty"`
}

// ErrorDTO is the JSON body returned on error.
type ErrorDTO struct {
	Error string `json:"error"`
}

// toMessageDTO renders a message, inlining textual parts small enough to fit.
func toMessageDTO(ctx context.Context, message courier.Message, inlineTextLimit int64) (*MessageDTO, error) {
	dto := &MessageDTO{
		ID:       string(message.ID()),
		SentAt:   message.SentAt(),
		Channel:  toChannelDTO(message.Channel()),
		From:     toUserDTO(message.From()),
		Mentions: toMentionDTOs(courier.Mentions(message)),
		Parts:    make([]PartDTO, 0, len(message.Parts())),
	}

	if parent, ok := courier.InReplyTo(message); ok {
		dto.InReplyTo = string(parent)
	}

	for _, part := range message.Parts() {
		partDTO, err := toPartDTO(ctx, message.ID(), part, inlineTextLimit)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		dto.Parts = append(dto.Parts, *partDTO)
	}

	return dto, nil
}

func toPartDTO(ctx context.Context, messageID courier.MessageID, part courier.MessagePart, inlineTextLimit int64) (*PartDTO, error) {
	kind := courier.MediaKindOf(part.ContentType())

	dto := &PartDTO{
		Name:        part.Name(),
		ContentType: part.ContentType(),
		Kind:        string(kind),
		URL:         partURL(messageID, part.Name()),
	}

	if attachment, ok := part.(courier.Attachment); ok {
		dto.Filename = courier.FilenameFor(attachment)
		dto.Disposition = string(attachment.Disposition())
		dto.Caption = attachment.Caption()

		if size := attachment.Size(); size >= 0 {
			dto.Size = size
		}
	}

	if voiceNote, ok := part.(courier.VoiceNote); ok && voiceNote.IsVoiceNote() {
		dto.VoiceNote = true
		dto.DurationMs = voiceNote.Duration().Milliseconds()
	}

	// Only textual parts are inlined, and only when small enough. Attachments
	// keep their URL so that clients decide whether to download them.
	if kind != courier.MediaKindText || dto.Disposition == string(courier.DispositionAttachment) {
		return dto, nil
	}

	if dto.Size > 0 && dto.Size > inlineTextLimit {
		return dto, nil
	}

	content, err := courier.ReadPart(ctx, part)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if int64(len(content)) > inlineTextLimit {
		return dto, nil
	}

	dto.Content = string(content)

	return dto, nil
}

func toChannelDTO(channel courier.Channel) ChannelDTO {
	if channel == nil {
		return ChannelDTO{}
	}

	return ChannelDTO{
		ID:   string(channel.ChannelID()),
		Kind: string(channel.Kind()),
		Name: channel.Name(),
	}
}

func toUserDTO(user courier.User) UserDTO {
	if user == nil {
		return UserDTO{}
	}

	return UserDTO{
		ID:          string(user.ID()),
		DisplayName: user.DisplayName(),
	}
}

func toMentionDTOs(mentions []courier.Mention) []MentionDTO {
	if len(mentions) == 0 {
		return nil
	}

	dtos := make([]MentionDTO, 0, len(mentions))

	for _, m := range mentions {
		dtos = append(dtos, MentionDTO{
			UserID:      string(m.UserID),
			DisplayName: m.DisplayName,
		})
	}

	return dtos
}

func toMentions(dtos []MentionDTO) []courier.Mention {
	mentions := make([]courier.Mention, 0, len(dtos))

	for _, dto := range dtos {
		mentions = append(mentions, courier.Mention{
			UserID:      courier.UserID(dto.UserID),
			DisplayName: dto.DisplayName,
		})
	}

	return mentions
}

func partURL(messageID courier.MessageID, partName string) string {
	return fmt.Sprintf("/messages/%s/parts/%s", urlEscape(string(messageID)), urlEscape(partName))
}
