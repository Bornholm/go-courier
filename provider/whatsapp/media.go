package whatsapp

import (
	"bytes"
	"context"
	"io"
	"time"

	"github.com/bornholm/go-courier"
	"github.com/pkg/errors"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
)

// mediaMessage describes the media carried by a WhatsApp message, normalized
// across the protobuf types so that building a courier attachment is uniform.
type mediaMessage struct {
	downloadable whatsmeow.DownloadableMessage
	mimeType     string
	filename     string
	caption      string
	size         int64
	duration     time.Duration
	voiceNote    bool
}

// mediaOf extracts the media of a WhatsApp message, if any. WhatsApp carries
// at most one media per message.
func mediaOf(msg *waE2E.Message) (*mediaMessage, bool) {
	if msg == nil {
		return nil, false
	}

	switch {
	case msg.GetImageMessage() != nil:
		image := msg.GetImageMessage()

		return &mediaMessage{
			downloadable: image,
			mimeType:     image.GetMimetype(),
			caption:      image.GetCaption(),
			size:         int64(image.GetFileLength()),
		}, true

	case msg.GetAudioMessage() != nil:
		audio := msg.GetAudioMessage()

		return &mediaMessage{
			downloadable: audio,
			mimeType:     audio.GetMimetype(),
			size:         int64(audio.GetFileLength()),
			duration:     time.Duration(audio.GetSeconds()) * time.Second,
			// PTT stands for push to talk: this is a voice note rather than a
			// shared audio file, and the prime candidate for transcription.
			voiceNote: audio.GetPTT(),
		}, true

	case msg.GetVideoMessage() != nil:
		video := msg.GetVideoMessage()

		return &mediaMessage{
			downloadable: video,
			mimeType:     video.GetMimetype(),
			caption:      video.GetCaption(),
			size:         int64(video.GetFileLength()),
			duration:     time.Duration(video.GetSeconds()) * time.Second,
		}, true

	case msg.GetDocumentMessage() != nil:
		document := msg.GetDocumentMessage()

		filename := document.GetFileName()
		if filename == "" {
			filename = document.GetTitle()
		}

		return &mediaMessage{
			downloadable: document,
			mimeType:     document.GetMimetype(),
			filename:     filename,
			caption:      document.GetCaption(),
			size:         int64(document.GetFileLength()),
		}, true

	case msg.GetStickerMessage() != nil:
		sticker := msg.GetStickerMessage()

		return &mediaMessage{
			downloadable: sticker,
			mimeType:     sticker.GetMimetype(),
			size:         int64(sticker.GetFileLength()),
		}, true
	}

	return nil, false
}

// toAttachment turns a WhatsApp media into a courier attachment. Nothing is
// downloaded here: the content is fetched on first read, and only if the
// application actually reads it.
func (p *Provider) toAttachment(client *whatsmeow.Client, media *mediaMessage) courier.Attachment {
	mimeType := media.mimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	download := func(ctx context.Context) (io.ReadCloser, error) {
		data, err := client.Download(ctx, media.downloadable)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		return io.NopCloser(bytes.NewReader(data)), nil
	}

	// Buffered so the media survives being read more than once, for instance
	// transcribed and then archived.
	open, release := courier.BufferedOpener(download, p.opts.MaxInMemorySize)
	p.trackRelease(release)

	funcs := []courier.AttachmentOptionFunc{
		courier.WithAttachmentName(attachmentPartName),
		courier.WithAttachmentCaption(media.caption),
	}

	if media.size > 0 {
		funcs = append(funcs, courier.WithAttachmentSize(media.size))
	}

	switch {
	case media.voiceNote:
		funcs = append(funcs, courier.WithAttachmentVoiceNote(media.duration))
	case media.duration > 0:
		funcs = append(funcs, courier.WithAttachmentDuration(media.duration))
	}

	return courier.NewAttachment(media.filename, mimeType, open, funcs...)
}

// mediaTypeFor maps a content type to the WhatsApp upload bucket it belongs
// to.
func mediaTypeFor(contentType string) whatsmeow.MediaType {
	switch courier.MediaKindOf(contentType) {
	case courier.MediaKindImage:
		return whatsmeow.MediaImage
	case courier.MediaKindAudio:
		return whatsmeow.MediaAudio
	case courier.MediaKindVideo:
		return whatsmeow.MediaVideo
	default:
		return whatsmeow.MediaDocument
	}
}

// buildMediaMessage uploads an attachment and wraps it in the protobuf
// message matching its kind. caption is only carried by the types supporting
// it.
func buildMediaMessage(ctx context.Context, client *whatsmeow.Client, attachment courier.Attachment, caption string) (*waE2E.Message, error) {
	data, err := courier.ReadPart(ctx, attachment)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	contentType := attachment.ContentType()
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	uploaded, err := client.Upload(ctx, data, mediaTypeFor(contentType))
	if err != nil {
		return nil, errors.WithStack(err)
	}

	var (
		url        = uploaded.URL
		directPath = uploaded.DirectPath
		length     = uploaded.FileLength
	)

	switch courier.MediaKindOf(contentType) {
	case courier.MediaKindImage:
		return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			URL:           &url,
			DirectPath:    &directPath,
			MediaKey:      uploaded.MediaKey,
			Mimetype:      &contentType,
			FileEncSHA256: uploaded.FileEncSHA256,
			FileSHA256:    uploaded.FileSHA256,
			FileLength:    &length,
			Caption:       optionalString(caption),
		}}, nil

	case courier.MediaKindAudio:
		audio := &waE2E.AudioMessage{
			URL:           &url,
			DirectPath:    &directPath,
			MediaKey:      uploaded.MediaKey,
			Mimetype:      &contentType,
			FileEncSHA256: uploaded.FileEncSHA256,
			FileSHA256:    uploaded.FileSHA256,
			FileLength:    &length,
		}

		if voiceNote, ok := attachment.(courier.VoiceNote); ok && voiceNote.IsVoiceNote() {
			ptt := true
			seconds := uint32(voiceNote.Duration().Seconds())

			audio.PTT = &ptt
			audio.Seconds = &seconds
		}

		return &waE2E.Message{AudioMessage: audio}, nil

	case courier.MediaKindVideo:
		return &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
			URL:           &url,
			DirectPath:    &directPath,
			MediaKey:      uploaded.MediaKey,
			Mimetype:      &contentType,
			FileEncSHA256: uploaded.FileEncSHA256,
			FileSHA256:    uploaded.FileSHA256,
			FileLength:    &length,
			Caption:       optionalString(caption),
		}}, nil

	default:
		filename := courier.FilenameFor(attachment)

		return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
			URL:           &url,
			DirectPath:    &directPath,
			MediaKey:      uploaded.MediaKey,
			Mimetype:      &contentType,
			FileEncSHA256: uploaded.FileEncSHA256,
			FileSHA256:    uploaded.FileSHA256,
			FileLength:    &length,
			FileName:      &filename,
			Caption:       optionalString(caption),
		}}, nil
	}
}

// optionalString returns nil for an empty string, so that empty captions are
// left out of the protobuf message rather than sent as empty ones.
func optionalString(value string) *string {
	if value == "" {
		return nil
	}

	return &value
}

// attachmentPartName is the part name given to the single media a WhatsApp
// message may carry.
const attachmentPartName = "att-0"
