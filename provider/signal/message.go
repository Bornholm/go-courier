package signal

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/pkg/errors"

	"github.com/bornholm/go-courier"
)

// receiveParams mirrors the params of a "receive" JSON-RPC notification.
// Only the fields this provider consumes are declared.
type receiveParams struct {
	Account  string `json:"account"`
	Envelope struct {
		Source     string `json:"source"`
		SourceUUID string `json:"sourceUuid"`
		SourceName string `json:"sourceName"`
		Timestamp  int64  `json:"timestamp"`

		DataMessage *dataMessage `json:"dataMessage"`
	} `json:"envelope"`
}

type dataMessage struct {
	Message   string `json:"message"`
	Timestamp int64  `json:"timestamp"`

	GroupInfo *struct {
		GroupID string `json:"groupId"`
	} `json:"groupInfo"`

	Mentions []struct {
		Name   string `json:"name"`
		Number string `json:"number"`
		UUID   string `json:"uuid"`
	} `json:"mentions"`

	// Quote references the replied-to message. Signal identifies messages
	// by sender timestamp: id is a number from signal-cli itself, but kept
	// loose (any) so bridge variants emitting strings still parse — liberal
	// reader, strict writer.
	Quote *struct {
		ID     any    `json:"id"`
		Author string `json:"author"`
	} `json:"quote"`

	Attachments []attachmentMeta `json:"attachments"`

	// Previews carries the link preview cards the sender's client generated
	// for URLs in the message. The image, when present, is a regular
	// attachment fetchable through getAttachment.
	Previews []struct {
		URL         string          `json:"url"`
		Title       string          `json:"title"`
		Description string          `json:"description"`
		Image       *attachmentMeta `json:"image"`
	} `json:"previews"`
}

// attachmentMeta mirrors the attachment description of a "receive"
// notification, shared by message attachments and link preview images.
type attachmentMeta struct {
	ID          string `json:"id"`
	ContentType string `json:"contentType"`
	Filename    string `json:"filename"`
	Size        int64  `json:"size"`
	// VoiceNote distingue un mémo vocal d'un simple fichier audio.
	VoiceNote bool `json:"voiceNote"`
}

// toMessage converts a receive notification to a courier.Message. ok is
// false for envelopes carrying no user-facing content: delivery receipts,
// typing events, sync messages...
func (p *Provider) toMessage(raw json.RawMessage) (courier.Message, bool) {
	var params receiveParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, false
	}

	data := params.Envelope.DataMessage
	if data == nil {
		return nil, false
	}
	if data.Message == "" && len(data.Attachments) == 0 {
		return nil, false
	}

	// Direct conversation: the channel is the PEER, so replies go back to
	// them. Group: the group id, prefixed to disambiguate.
	from := courier.NewUser(courier.UserID(params.Envelope.Source), displayName(params.Envelope.SourceName, params.Envelope.Source))

	var channel courier.Channel
	if data.GroupInfo != nil && data.GroupInfo.GroupID != "" {
		channel = courier.NewChannel(courier.ChannelID(groupChannelPrefix+data.GroupInfo.GroupID), courier.ChannelKindGroup, data.GroupInfo.GroupID)
	} else {
		channel = courier.NewChannel(courier.ChannelID(params.Envelope.Source), courier.ChannelKindDirect, from.DisplayName())
	}

	options := []courier.BaseMessageOptionFunc{
		courier.WithMessageSentAt(time.UnixMilli(params.Envelope.Timestamp)),
	}
	if data.Message != "" {
		options = append(options, courier.WithMessageMainPart(data.Message))
	}

	for _, m := range data.Mentions {
		id := m.Number
		if id == "" {
			id = m.UUID
		}
		options = append(options, courier.WithMessageMentions(courier.Mention{
			UserID:      courier.UserID(id),
			DisplayName: m.Name,
		}))
	}

	if data.Quote != nil {
		options = append(options, courier.WithMessageInReplyTo(quotedMessageID(data.Quote.ID, data.Quote.Author)))
	}

	for i, attachment := range data.Attachments {
		options = append(options, courier.WithMessagePart(p.toAttachment(i, channel.ChannelID(), params.Envelope.Source, attachment)))
	}

	for i, raw := range data.Previews {
		if raw.URL == "" {
			continue
		}

		preview := courier.LinkPreview{
			URL:         raw.URL,
			Title:       raw.Title,
			Description: raw.Description,
		}

		// The preview image stays out of the message parts: it describes the
		// linked page, it is not a file the sender attached.
		if raw.Image != nil {
			preview.Thumbnail = p.toAttachment(i, channel.ChannelID(), params.Envelope.Source, *raw.Image)
		}

		options = append(options, courier.WithMessageLinkPreviews(preview))
	}

	// Signal identifies a message by its sender timestamp; unique per
	// (sender, timestamp), which is enough for deduplication downstream.
	id := courier.MessageID(fmt.Sprintf("%s:%d", params.Envelope.Source, messageTimestamp(params)))

	return courier.NewMessage(id, channel, from, options...), true
}

// quotedMessageID rebuilds the courier MessageID of a quoted message, in
// the same "<sender>:<timestamp>" form given to incoming messages, so that
// InReplyTo actually matches the ID the application saw earlier.
func quotedMessageID(id any, author string) courier.MessageID {
	switch v := id.(type) {
	case float64:
		// JSON numbers land as float64; a millisecond epoch stays exact
		// well below the 2^53 integer limit.
		return courier.MessageID(fmt.Sprintf("%s:%d", author, int64(v)))
	case string:
		return courier.MessageID(v)
	default:
		return courier.MessageID(fmt.Sprintf("%s:%v", author, v))
	}
}

func messageTimestamp(params receiveParams) int64 {
	if params.Envelope.DataMessage.Timestamp != 0 {
		return params.Envelope.DataMessage.Timestamp
	}
	return params.Envelope.Timestamp
}

func displayName(name, fallback string) string {
	if name != "" {
		return name
	}
	return fallback
}

// toAttachment builds a lazy attachment: bytes are fetched through the
// getAttachment RPC method on first read, never at reception. Replayable by
// construction — the daemon keeps attachment files on disk, each Reader call
// fetches them again.
func (p *Provider) toAttachment(index int, channelID courier.ChannelID, sender string, meta attachmentMeta) courier.Attachment {
	contentType := meta.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	filename := meta.Filename
	if filename == "" {
		filename = fmt.Sprintf("attachment-%d", index)
	}

	open := func(ctx context.Context) (io.ReadCloser, error) {
		client, err := p.connect(ctx)
		if err != nil {
			return nil, err
		}

		params := p.params()
		params["id"] = meta.ID
		// getAttachment addresses the CONVERSATION the attachment arrived
		// in: the group for a group message, the sender otherwise.
		addressAttachmentParams(params, channelID, sender)

		result, err := client.call(ctx, "getAttachment", params)
		if err != nil {
			return nil, errors.Wrapf(err, "could not fetch attachment %q", meta.ID)
		}

		var payload struct {
			Data string `json:"data"`
		}
		if err := json.Unmarshal(result, &payload); err != nil {
			return nil, errors.WithStack(err)
		}

		data, err := base64.StdEncoding.DecodeString(payload.Data)
		if err != nil {
			return nil, errors.Wrap(err, "could not decode attachment data")
		}

		return io.NopCloser(bytes.NewReader(data)), nil
	}

	attachmentOptions := []courier.AttachmentOptionFunc{
		courier.WithAttachmentName(fmt.Sprintf("att-%d", index)),
		courier.WithAttachmentSize(meta.Size),
	}
	if meta.VoiceNote {
		attachmentOptions = append(attachmentOptions, courier.WithAttachmentVoiceNote(0))
	}

	return courier.NewAttachment(filename, contentType, open, attachmentOptions...)
}

// addressAttachmentParams routes a getAttachment call to the right
// conversation.
func addressAttachmentParams(params map[string]any, channelID courier.ChannelID, sender string) {
	id := string(channelID)
	if groupID, ok := cutGroupPrefix(id); ok {
		params["groupId"] = groupID
		return
	}
	params["recipient"] = sender
}

func cutGroupPrefix(id string) (string, bool) {
	if len(id) > len(groupChannelPrefix) && id[:len(groupChannelPrefix)] == groupChannelPrefix {
		return id[len(groupChannelPrefix):], true
	}
	return "", false
}
