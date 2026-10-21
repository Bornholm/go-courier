package whatsappcloud

import (
	"context"
	"io"
	"log/slog"
	"mime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bornholm/go-courier"
	"github.com/pkg/errors"
)

// ErrUnsupportedMedia is returned by Send for an attachment whose type the
// Cloud API does not accept. The application may share a link instead.
var ErrUnsupportedMedia = errors.New("media type not supported by whatsapp")

// attachmentPartName names the media part of incoming messages, as the
// whatsmeow provider does: WhatsApp carries at most one media per message.
const attachmentPartName = "att-0"

// Limits of the Cloud API.
const (
	maxTextLength    = 4096
	maxCaptionLength = 1024
)

// toMessage converts a webhook message, returning nil when it carries
// nothing worth forwarding.
func (p *Provider) toMessage(ctx context.Context, raw webhookMessage, channel courier.Channel, from courier.User, sentAt time.Time) courier.Message {
	text := ""
	funcs := []courier.BaseMessageOptionFunc{courier.WithMessageSentAt(sentAt)}

	switch {
	case raw.Text != nil:
		text = raw.Text.Body
	case raw.Button != nil:
		// The quick reply button of a template.
		text = raw.Button.Text
	case raw.Interactive != nil && raw.Interactive.ButtonReply != nil:
		text = raw.Interactive.ButtonReply.Title
	case raw.Interactive != nil && raw.Interactive.ListReply != nil:
		text = raw.Interactive.ListReply.Title
	case raw.Location != nil:
		text = describeLocation(raw.Location.Name, raw.Location.Address, raw.Location.Latitude, raw.Location.Longitude)
	}

	if media, voice := mediaOf(raw); media != nil {
		funcs = append(funcs, courier.WithMessagePart(p.toAttachment(media, voice)))
		if text == "" {
			text = media.Caption
		}
	} else if text == "" {
		return nil
	}

	funcs = append(funcs, courier.WithMessageMainPart(text))

	self, err := p.Self(ctx)
	if err != nil {
		slog.DebugContext(ctx, "could not resolve whatsapp business identity", slog.Any("error", err))
		self = nil
	}

	if mentions := mentionsOf(text, self); len(mentions) > 0 {
		funcs = append(funcs, courier.WithMessageMentions(mentions...))
	}

	if raw.Context != nil && raw.Context.ID != "" {
		funcs = append(funcs, courier.WithMessageInReplyTo(courier.MessageID(raw.Context.ID)))
	}

	return courier.NewMessage(courier.MessageID(raw.ID), channel, from, funcs...)
}

func mediaOf(raw webhookMessage) (*webhookMedia, bool) {
	switch {
	case raw.Audio != nil:
		return raw.Audio, raw.Audio.Voice
	case raw.Image != nil:
		return raw.Image, false
	case raw.Video != nil:
		return raw.Video, false
	case raw.Document != nil:
		return raw.Document, false
	case raw.Sticker != nil:
		return raw.Sticker, false
	default:
		return nil, false
	}
}

// toAttachment turns a webhook media into an attachment. Nothing is
// downloaded here: the content is fetched on first read, and only if the
// application reads it.
func (p *Provider) toAttachment(media *webhookMedia, voice bool) courier.Attachment {
	contentType := media.MimeType
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	mediaID := media.ID
	download := func(ctx context.Context) (io.ReadCloser, error) {
		return p.downloadMedia(ctx, mediaID)
	}

	// Buffered so the media survives being read more than once, for
	// instance transcribed and then archived.
	open, release := courier.BufferedOpener(download, p.opts.MaxInMemorySize)
	p.trackRelease(release)

	funcs := []courier.AttachmentOptionFunc{
		courier.WithAttachmentName(attachmentPartName),
		courier.WithAttachmentCaption(media.Caption),
	}
	if voice {
		// The Cloud API does not report the duration of a voice note.
		funcs = append(funcs, courier.WithAttachmentVoiceNote(0))
	}

	return courier.NewAttachment(media.Filename, contentType, open, funcs...)
}

// deliver sends message as free-form messages, regardless of the service
// window. A message split over several WhatsApp messages returns the
// identifier of the first, the one carrying the text: it is the one a user
// reacts to when judging the answer.
func (p *Provider) deliver(ctx context.Context, message courier.Message) (courier.MessageID, error) {
	recipient := message.Channel().ChannelID()

	content, err := courier.GetMessageMainContent(ctx, message)
	if err != nil && !errors.Is(err, courier.ErrNotFound) {
		return "", errors.WithStack(err)
	}

	replyTo, _ := courier.InReplyTo(message)
	attachments := courier.Attachments(message)

	if len(attachments) == 0 && strings.TrimSpace(content) == "" {
		return "", errors.New("message has neither content nor attachment")
	}

	var first courier.MessageID

	post := func(payload outgoing) error {
		if first == "" && replyTo != "" {
			payload.Context = &outgoingContext{MessageID: string(replyTo)}
		}

		id, err := p.postMessage(ctx, payload)
		if err != nil {
			return errors.WithStack(err)
		}
		if first == "" {
			first = courier.MessageID(id)
		}
		return nil
	}

	// The text rides as the caption of the first media when it can: an
	// audio has none, and a caption is shorter than a text message.
	caption := ""
	if len(attachments) > 0 && content != "" && captionable(attachments[0]) && utf8.RuneCountInString(content) <= maxCaptionLength {
		caption = content
		content = ""
	}

	for _, chunk := range splitText(content, maxTextLength) {
		payload := addressed(recipient)
		payload.Type = "text"
		payload.Text = &outgoingText{Body: chunk, PreviewURL: strings.Contains(chunk, "://")}

		if err := post(payload); err != nil {
			return "", errors.WithStack(err)
		}
	}

	for idx, attachment := range attachments {
		payload, err := p.mediaPayload(ctx, recipient, attachment)
		if err != nil {
			return first, errors.WithStack(err)
		}

		if idx == 0 && caption != "" {
			mediaOfPayload(&payload).Caption = caption
		}

		if err := post(payload); err != nil {
			return first, errors.WithStack(err)
		}
	}

	return first, nil
}

// mediaPayload uploads attachment and returns the message referencing it.
func (p *Provider) mediaPayload(ctx context.Context, recipient courier.ChannelID, attachment courier.Attachment) (outgoing, error) {
	kind, uploadType, ok := cloudMediaKind(attachment.ContentType())
	if !ok {
		return outgoing{}, errors.Wrapf(ErrUnsupportedMedia, "%q", essenceOf(attachment.ContentType()))
	}

	reader, err := attachment.Reader(ctx)
	if err != nil {
		return outgoing{}, errors.WithStack(err)
	}
	defer reader.Close()

	filename := courier.FilenameFor(attachment)

	mediaID, err := p.uploadMedia(ctx, filename, uploadType, reader)
	if err != nil {
		return outgoing{}, errors.WithStack(err)
	}

	payload := addressed(recipient)
	payload.Type = kind
	media := &outgoingMedia{ID: mediaID}

	switch kind {
	case "image":
		payload.Image = media
	case "video":
		payload.Video = media
	case "audio":
		// Only an Ogg/Opus recording renders as a voice note.
		media.Voice = courier.IsVoiceNote(attachment) && uploadType == "audio/ogg"
		payload.Audio = media
	default:
		media.Filename = filename
		payload.Document = media
	}

	return payload, nil
}

func mediaOfPayload(payload *outgoing) *outgoingMedia {
	switch {
	case payload.Image != nil:
		return payload.Image
	case payload.Video != nil:
		return payload.Video
	case payload.Audio != nil:
		return payload.Audio
	default:
		return payload.Document
	}
}

func captionable(attachment courier.Attachment) bool {
	kind, _, ok := cloudMediaKind(attachment.ContentType())
	return ok && kind != "audio"
}

// cloudMediaKind maps a content type to the message type carrying it and to
// the content type declared at upload. The Cloud API only accepts a short
// list of types.
func cloudMediaKind(contentType string) (kind, uploadType string, ok bool) {
	essence := essenceOf(contentType)

	switch essence {
	case "image/jpeg", "image/png":
		return "image", essence, true
	case "video/mp4", "video/3gpp":
		return "video", essence, true
	case "audio/aac", "audio/amr", "audio/mpeg", "audio/mp4", "audio/ogg":
		return "audio", essence, true
	case "application/pdf",
		"application/msword",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.ms-excel",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/vnd.ms-powerpoint",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"text/plain":
		return "document", essence, true
	}

	// Any text file reaches the user as a plain text document, its
	// extension kept in the filename.
	if strings.HasPrefix(essence, "text/") {
		return "document", "text/plain", true
	}

	return "", "", false
}

func essenceOf(contentType string) string {
	essence, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		essence = strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	}
	return essence
}

// splitText cuts text into chunks of at most limit runes, preferably at a
// paragraph, then a line, then a word boundary.
func splitText(text string, limit int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	var chunks []string

	for utf8.RuneCountInString(text) > limit {
		runes := []rune(text)
		window := string(runes[:limit])

		cut := -1
		for _, sep := range []string{"\n\n", "\n", " "} {
			if idx := strings.LastIndex(window, sep); idx > len(window)/2 {
				cut = idx
				break
			}
		}
		if cut < 0 {
			cut = len(window)
		}

		chunks = append(chunks, strings.TrimSpace(text[:cut]))
		text = strings.TrimSpace(text[cut:])
	}

	if text != "" {
		chunks = append(chunks, text)
	}

	return chunks
}
