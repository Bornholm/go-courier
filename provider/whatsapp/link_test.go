package whatsapp

import (
	"context"
	"testing"

	"github.com/bornholm/go-courier"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestLinkPreviewsOf(t *testing.T) {
	for _, tc := range []struct {
		name     string
		message  *waE2E.Message
		expected int
	}{
		{
			name: "link share with preview card",
			message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text:        proto.String("https://facebook.com/reel/123"),
				MatchedText: proto.String("https://facebook.com/reel/123"),
				Title:       proto.String("438 vues | Reel"),
				Description: proto.String("A reel"),
			}},
			expected: 1,
		},
		{
			name: "bare URL without preview card",
			message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text:        proto.String("look at https://example.com"),
				MatchedText: proto.String("https://example.com"),
			}},
			expected: 0,
		},
		{
			name: "plain text",
			message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: proto.String("hello"),
			}},
			expected: 0,
		},
		{
			name:     "conversation message",
			message:  &waE2E.Message{Conversation: proto.String("hello")},
			expected: 0,
		},
		{
			name:     "nil message",
			message:  nil,
			expected: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previews := linkPreviewsOf(tc.message)
			if len(previews) != tc.expected {
				t.Fatalf("linkPreviewsOf: got %d previews, expected %d", len(previews), tc.expected)
			}
		})
	}
}

func TestLinkPreviewsOfThumbnail(t *testing.T) {
	thumbnail := []byte{0xff, 0xd8, 0xff}

	previews := linkPreviewsOf(&waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text:          proto.String("https://example.com"),
		MatchedText:   proto.String("https://example.com"),
		Title:         proto.String("Example"),
		JPEGThumbnail: thumbnail,
	}})
	if len(previews) != 1 {
		t.Fatalf("linkPreviewsOf: got %d previews, expected 1", len(previews))
	}

	preview := previews[0]
	if preview.URL != "https://example.com" || preview.Title != "Example" {
		t.Errorf("preview = %+v", preview)
	}
	if preview.Thumbnail == nil {
		t.Fatal("expected a thumbnail part")
	}
	if preview.Thumbnail.ContentType() != "image/jpeg" {
		t.Errorf("thumbnail content type = %q", preview.Thumbnail.ContentType())
	}

	data, err := courier.ReadPart(context.Background(), preview.Thumbnail)
	if err != nil {
		t.Fatalf("reading thumbnail: %+v", err)
	}
	if string(data) != string(thumbnail) {
		t.Errorf("thumbnail bytes = %v, expected %v", data, thumbnail)
	}
}
