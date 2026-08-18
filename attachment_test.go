package courier

import (
	"context"
	"testing"
	"time"
)

func TestMediaKindOf(t *testing.T) {
	for _, tc := range []struct {
		contentType string
		expected    MediaKind
	}{
		{"image/png", MediaKindImage},
		{"image/jpeg", MediaKindImage},
		{"audio/ogg; codecs=opus", MediaKindAudio},
		{"AUDIO/OGG", MediaKindAudio},
		{"video/mp4", MediaKindVideo},
		{"text/plain; charset=utf-8", MediaKindText},
		{"text/html", MediaKindText},
		{"application/pdf", MediaKindDocument},
		{"application/json", MediaKindDocument},
		{"application/octet-stream", MediaKindOther},
		{"", MediaKindOther},
		{"not a content type", MediaKindOther},
	} {
		t.Run(tc.contentType, func(t *testing.T) {
			if got := MediaKindOf(tc.contentType); got != tc.expected {
				t.Errorf("MediaKindOf(%q): got %q, expected %q", tc.contentType, got, tc.expected)
			}
		})
	}
}

func TestAttachments(t *testing.T) {
	ctx := context.Background()

	voice := NewAttachment(
		"note.ogg", "audio/ogg", OpenerFromString("audio"),
		WithAttachmentVoiceNote(4*time.Second),
		WithAttachmentSize(5),
	)
	picture := NewAttachment("cat.png", "image/png", OpenerFromString("png"))

	message := NewMessage(
		"msg-1",
		NewChannel("chan-1", ChannelKindDirect, "direct"),
		NewUser("user-1", "User"),
		WithMessageMainPart("hello"),
		WithMessagePart(voice),
		WithMessagePart(picture),
	)

	attachments := Attachments(message)
	if len(attachments) != 2 {
		t.Fatalf("len(Attachments(message)): got %d, expected 2", len(attachments))
	}

	// The main part is not an attachment.
	if attachments[0].Name() != "note.ogg" {
		t.Errorf("attachments[0].Name(): got %q, expected %q", attachments[0].Name(), "note.ogg")
	}

	audio := AttachmentsByKind(message, MediaKindAudio)
	if len(audio) != 1 {
		t.Fatalf("len(AttachmentsByKind(message, MediaKindAudio)): got %d, expected 1", len(audio))
	}

	if !IsVoiceNote(audio[0]) {
		t.Error("IsVoiceNote(audio[0]): got false, expected true")
	}

	if IsVoiceNote(picture) {
		t.Error("IsVoiceNote(picture): got true, expected false")
	}

	if got := audio[0].Size(); got != 5 {
		t.Errorf("audio[0].Size(): got %d, expected 5", got)
	}

	// Size defaults to -1, meaning unknown.
	if got := picture.Size(); got != -1 {
		t.Errorf("picture.Size(): got %d, expected -1", got)
	}

	if got := picture.Disposition(); got != DispositionAttachment {
		t.Errorf("picture.Disposition(): got %q, expected %q", got, DispositionAttachment)
	}

	content, err := ReadPart(ctx, audio[0])
	if err != nil {
		t.Fatalf("ReadPart(ctx, audio[0]): %+v", err)
	}

	if string(content) != "audio" {
		t.Errorf("ReadPart(ctx, audio[0]): got %q, expected %q", string(content), "audio")
	}
}

func TestFilenameFor(t *testing.T) {
	named := NewAttachment("report.pdf", "application/pdf", nil)
	if got := FilenameFor(named); got != "report.pdf" {
		t.Errorf("FilenameFor(named): got %q, expected %q", got, "report.pdf")
	}

	// Without a filename, one is derived from the part name and content type.
	unnamed := NewAttachment("", "image/png", nil, WithAttachmentName("att-0"))
	if got := FilenameFor(unnamed); got != "att-0.png" {
		t.Errorf("FilenameFor(unnamed): got %q, expected %q", got, "att-0.png")
	}
}
