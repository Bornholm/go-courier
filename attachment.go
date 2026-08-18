package courier

import (
	"context"
	"io"
	"mime"
	"strings"
	"time"

	"github.com/pkg/errors"
)

// Disposition tells how an attachment is meant to be presented.
type Disposition string

const (
	// DispositionAttachment is a file joined to the message.
	DispositionAttachment Disposition = "attachment"
	// DispositionInline is content embedded in the message body, such as a
	// CID referenced image in an email or an alternative HTML body.
	DispositionInline Disposition = "inline"
)

// MediaKind is a coarse classification of a content type. Its values match
// the multimodal attachment types expected by LLM clients, so that converting
// a courier attachment into an LLM one is a direct mapping.
type MediaKind string

const (
	MediaKindImage    MediaKind = "image"
	MediaKindAudio    MediaKind = "audio"
	MediaKindVideo    MediaKind = "video"
	MediaKindDocument MediaKind = "document"
	MediaKindText     MediaKind = "text"
	MediaKindOther    MediaKind = "other"
)

// documentTypes are the non text, non media content types worth treating as
// documents rather than opaque blobs.
var documentTypes = map[string]struct{}{
	"application/pdf":    {},
	"application/msword": {},
	"application/rtf":    {},
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   {},
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         {},
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": {},
	"application/vnd.oasis.opendocument.text":                                   {},
	"application/vnd.oasis.opendocument.spreadsheet":                            {},
	"application/vnd.oasis.opendocument.presentation":                           {},
	"application/vnd.ms-excel":                                                  {},
	"application/vnd.ms-powerpoint":                                             {},
	"application/epub+zip":                                                      {},
	"application/json":                                                          {},
	"application/xml":                                                           {},
}

// MediaKindOf classifies a content type, ignoring its parameters. For
// instance "audio/ogg; codecs=opus" is classified as MediaKindAudio.
func MediaKindOf(contentType string) MediaKind {
	essence, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		// Not a well formed content type, fall back on the raw value so that
		// slightly malformed headers still classify correctly.
		essence = strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	}

	switch {
	case strings.HasPrefix(essence, "image/"):
		return MediaKindImage
	case strings.HasPrefix(essence, "audio/"):
		return MediaKindAudio
	case strings.HasPrefix(essence, "video/"):
		return MediaKindVideo
	case strings.HasPrefix(essence, "text/"):
		return MediaKindText
	}

	if _, ok := documentTypes[essence]; ok {
		return MediaKindDocument
	}

	return MediaKindOther
}

// Attachment is a message part carrying a file rather than the message body.
type Attachment interface {
	MessagePart
	// Filename is the name the file had on the sending platform, possibly
	// empty.
	Filename() string
	// Size is the content length in bytes, or -1 when unknown.
	Size() int64
	Disposition() Disposition
	// Caption is the text the sender attached to the file, for platforms
	// supporting it. Empty otherwise.
	Caption() string
}

// VoiceNote is an optional interface implemented by attachments known to be
// voice recordings rather than plain audio files. Detect it with a type
// assertion, or use the IsVoiceNote helper.
type VoiceNote interface {
	Attachment
	Duration() time.Duration
	IsVoiceNote() bool
}

type BaseAttachment struct {
	name        string
	filename    string
	contentType string
	open        PartOpener
	size        int64
	disposition Disposition
	caption     string
	duration    time.Duration
	voiceNote   bool
}

// Name implements MessagePart.
func (a *BaseAttachment) Name() string {
	return a.name
}

// ContentType implements MessagePart.
func (a *BaseAttachment) ContentType() string {
	return a.contentType
}

// Reader implements MessagePart.
func (a *BaseAttachment) Reader(ctx context.Context) (io.ReadCloser, error) {
	if a.open == nil {
		return nil, errors.WithStack(ErrNotFound)
	}

	reader, err := a.open(ctx)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return reader, nil
}

// Filename implements Attachment.
func (a *BaseAttachment) Filename() string {
	return a.filename
}

// Size implements Attachment.
func (a *BaseAttachment) Size() int64 {
	return a.size
}

// Disposition implements Attachment.
func (a *BaseAttachment) Disposition() Disposition {
	return a.disposition
}

// Caption implements Attachment.
func (a *BaseAttachment) Caption() string {
	return a.caption
}

// Duration implements VoiceNote.
func (a *BaseAttachment) Duration() time.Duration {
	return a.duration
}

// IsVoiceNote implements VoiceNote.
func (a *BaseAttachment) IsVoiceNote() bool {
	return a.voiceNote
}

var (
	_ Attachment = &BaseAttachment{}
	_ VoiceNote  = &BaseAttachment{}
)

type BaseAttachmentOptions struct {
	Name        string
	Size        int64
	Disposition Disposition
	Caption     string
	Duration    time.Duration
	VoiceNote   bool
}

type AttachmentOptionFunc func(opts *BaseAttachmentOptions)

func NewBaseAttachmentOptions(funcs ...AttachmentOptionFunc) *BaseAttachmentOptions {
	opts := &BaseAttachmentOptions{
		Size:        -1,
		Disposition: DispositionAttachment,
	}
	for _, fn := range funcs {
		fn(opts)
	}
	return opts
}

// WithAttachmentName overrides the part name, which defaults to the filename.
// Part names should be unique within a message.
func WithAttachmentName(name string) AttachmentOptionFunc {
	return func(opts *BaseAttachmentOptions) {
		opts.Name = name
	}
}

func WithAttachmentSize(size int64) AttachmentOptionFunc {
	return func(opts *BaseAttachmentOptions) {
		opts.Size = size
	}
}

func WithAttachmentDisposition(disposition Disposition) AttachmentOptionFunc {
	return func(opts *BaseAttachmentOptions) {
		opts.Disposition = disposition
	}
}

func WithAttachmentCaption(caption string) AttachmentOptionFunc {
	return func(opts *BaseAttachmentOptions) {
		opts.Caption = caption
	}
}

// WithAttachmentVoiceNote flags the attachment as a voice recording and
// declares its duration.
func WithAttachmentVoiceNote(duration time.Duration) AttachmentOptionFunc {
	return func(opts *BaseAttachmentOptions) {
		opts.VoiceNote = true
		opts.Duration = duration
	}
}

// WithAttachmentDuration declares the duration of an audio or video
// attachment without flagging it as a voice recording.
func WithAttachmentDuration(duration time.Duration) AttachmentOptionFunc {
	return func(opts *BaseAttachmentOptions) {
		opts.Duration = duration
	}
}

func NewAttachment(filename string, contentType string, open PartOpener, funcs ...AttachmentOptionFunc) *BaseAttachment {
	opts := NewBaseAttachmentOptions(funcs...)

	name := opts.Name
	if name == "" {
		name = filename
	}

	return &BaseAttachment{
		name:        name,
		filename:    filename,
		contentType: contentType,
		open:        open,
		size:        opts.Size,
		disposition: opts.Disposition,
		caption:     opts.Caption,
		duration:    opts.Duration,
		voiceNote:   opts.VoiceNote,
	}
}

// Attachments returns the message parts that are attachments, in order.
func Attachments(message Message) []Attachment {
	attachments := make([]Attachment, 0, len(message.Parts()))

	for _, p := range message.Parts() {
		attachment, ok := p.(Attachment)
		if !ok {
			continue
		}

		attachments = append(attachments, attachment)
	}

	return attachments
}

// AttachmentsByKind returns the message attachments matching the given media
// kind.
func AttachmentsByKind(message Message, kind MediaKind) []Attachment {
	attachments := make([]Attachment, 0)

	for _, a := range Attachments(message) {
		if MediaKindOf(a.ContentType()) != kind {
			continue
		}

		attachments = append(attachments, a)
	}

	return attachments
}

// IsVoiceNote reports whether the part is a voice recording.
func IsVoiceNote(part MessagePart) bool {
	voiceNote, ok := part.(VoiceNote)
	if !ok {
		return false
	}

	return voiceNote.IsVoiceNote()
}

// FilenameFor returns the attachment filename, generating one from its
// content type when the platform did not provide any.
func FilenameFor(attachment Attachment) string {
	if filename := attachment.Filename(); filename != "" {
		return filename
	}

	name := attachment.Name()
	if name == "" {
		name = "attachment"
	}

	if extensions, err := mime.ExtensionsByType(attachment.ContentType()); err == nil && len(extensions) > 0 {
		return name + extensions[0]
	}

	return name
}
