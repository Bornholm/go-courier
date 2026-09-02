package courier

import "testing"

func TestLinkPreviews(t *testing.T) {
	preview := LinkPreview{
		URL:   "https://example.com/watch",
		Title: "Example video",
	}

	message := NewMessage("id", NewChannel("chan", ChannelKindDirect, "chan"), NewUser("user", "User"),
		WithMessageMainPart("https://example.com/watch"),
		WithMessageLinkPreviews(preview),
	)

	previews := LinkPreviews(message)
	if len(previews) != 1 {
		t.Fatalf("LinkPreviews: got %d previews, expected 1", len(previews))
	}
	if previews[0].URL != preview.URL || previews[0].Title != preview.Title {
		t.Errorf("LinkPreviews: got %+v, expected %+v", previews[0], preview)
	}

	// A message type that does not implement LinkPreviewMessage yields nil,
	// like the Mentions and InReplyTo helpers.
	if got := LinkPreviews(plainMessage{}); got != nil {
		t.Errorf("LinkPreviews on a non-implementing message: got %+v, expected nil", got)
	}
}

// plainMessage implements Message without any optional interface.
type plainMessage struct{ Message }
