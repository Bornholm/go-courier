package rocket

import (
	"context"
	"testing"

	"github.com/bornholm/go-courier"
)

func TestKindOf(t *testing.T) {
	for _, tc := range []struct {
		roomType string
		expected courier.ChannelKind
	}{
		{"d", courier.ChannelKindDirect},
		{"p", courier.ChannelKindGroup},
		{"c", courier.ChannelKindPublic},
		{"l", courier.ChannelKindPublic},
		{"", courier.ChannelKindUnknown},
	} {
		t.Run(tc.roomType, func(t *testing.T) {
			if got := kindOf(tc.roomType); got != tc.expected {
				t.Errorf("kindOf(%q): got %q, expected %q", tc.roomType, got, tc.expected)
			}
		})
	}
}

func TestAttachmentLink(t *testing.T) {
	for _, tc := range []struct {
		name        string
		attachment  Attachment
		link        string
		contentType string
	}{
		{
			name:        "title link",
			attachment:  Attachment{TitleLink: "/file-upload/abc/report.pdf"},
			link:        "/file-upload/abc/report.pdf",
			contentType: "",
		},
		{
			name:        "audio url and type",
			attachment:  Attachment{AudioURL: "/file-upload/abc/note.mp3", AudioType: "audio/mpeg"},
			link:        "/file-upload/abc/note.mp3",
			contentType: "audio/mpeg",
		},
		{
			name:       "no link at all",
			attachment: Attachment{Description: "just text"},
			link:       "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.attachment.link(); got != tc.link {
				t.Errorf("link(): got %q, expected %q", got, tc.link)
			}

			if got := tc.attachment.contentType(); got != tc.contentType {
				t.Errorf("contentType(): got %q, expected %q", got, tc.contentType)
			}
		})
	}
}

func TestToMessage(t *testing.T) {
	provider := NewProvider()
	defer provider.releaseAll()

	listener := &messageListener{ctx: context.Background(), provider: provider}

	messageInfo := MessageInfo{
		ID:        "msg-1",
		RoomID:    "room-1",
		Message:   "here is the file",
		ThreadID:  "thread-1",
		Timestamp: Timestamp{Date: 1_755_000_000_000},
		User:      UserInfo{ID: "user-1", Name: "Alice", Username: "alice"},
		Mentions:  []UserInfo{{ID: "bot", Username: "bot"}},
		Files: []FileInfo{{
			ID: "file-1", Name: "note.mp3", Type: "audio/mpeg", Size: 4096,
		}},
		Attachments: []Attachment{{
			Title:     "note.mp3",
			TitleLink: "/file-upload/file-1/note.mp3",
			AudioType: "audio/mpeg",
		}},
	}

	message := listener.toMessage(messageInfo, RoomInfo{Type: "p", Name: "Team"})

	if got := message.Channel().Kind(); got != courier.ChannelKindGroup {
		t.Errorf("channel kind: got %q, expected %q", got, courier.ChannelKindGroup)
	}

	if got := message.Channel().Name(); got != "Team" {
		t.Errorf("channel name: got %q, expected %q", got, "Team")
	}

	if got := message.From().DisplayName(); got != "Alice" {
		t.Errorf("from display name: got %q, expected %q", got, "Alice")
	}

	if !courier.IsMentioned(message, "bot") {
		t.Error("courier.IsMentioned(message, \"bot\"): got false, expected true")
	}

	parent, ok := courier.InReplyTo(message)
	if !ok || parent != "thread-1" {
		t.Errorf("courier.InReplyTo: got (%q, %v), expected (%q, true)", parent, ok, "thread-1")
	}

	if message.SentAt().IsZero() {
		t.Error("message.SentAt(): got zero time, expected the decoded timestamp")
	}

	attachments := courier.Attachments(message)
	if len(attachments) != 1 {
		t.Fatalf("len(courier.Attachments(message)): got %d, expected 1", len(attachments))
	}

	attachment := attachments[0]

	// Metadata comes from the file descriptor, which is more precise than the
	// rendering attachment.
	if got := courier.FilenameFor(attachment); got != "note.mp3" {
		t.Errorf("filename: got %q, expected %q", got, "note.mp3")
	}

	if got := attachment.ContentType(); got != "audio/mpeg" {
		t.Errorf("content type: got %q, expected %q", got, "audio/mpeg")
	}

	if got := attachment.Size(); got != 4096 {
		t.Errorf("size: got %d, expected 4096", got)
	}

	if got := courier.MediaKindOf(attachment.ContentType()); got != courier.MediaKindAudio {
		t.Errorf("media kind: got %q, expected %q", got, courier.MediaKindAudio)
	}
}
