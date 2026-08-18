package courier

import (
	"context"
	"testing"

	"github.com/pkg/errors"
)

func TestMessageMainContent(t *testing.T) {
	ctx := context.Background()

	message := NewMessage(
		"msg-1",
		NewChannelRef("chan-1"),
		NewUser("user-1", "User"),
		WithMessageMainPart("hello"),
	)

	content, err := GetMessageMainContent(ctx, message)
	if err != nil {
		t.Fatalf("GetMessageMainContent: %+v", err)
	}

	if content != "hello" {
		t.Errorf("GetMessageMainContent: got %q, expected %q", content, "hello")
	}

	// The main part must stay readable: providers and applications both read
	// it, and the REST provider replays it on every reconnection.
	again, err := GetMessageMainContent(ctx, message)
	if err != nil {
		t.Fatalf("second GetMessageMainContent: %+v", err)
	}

	if again != "hello" {
		t.Errorf("second GetMessageMainContent: got %q, expected %q", again, "hello")
	}
}

func TestMessageMainPartOfType(t *testing.T) {
	ctx := context.Background()

	message := NewMessage(
		"msg-1",
		NewChannelRef("chan-1"),
		NewUser("user-1", "User"),
		WithMessageMainPartOfType("<p>hello</p>", "text/html"),
	)

	main, err := GetMessageMainPart(message)
	if err != nil {
		t.Fatalf("GetMessageMainPart: %+v", err)
	}

	if got := main.ContentType(); got != "text/html" {
		t.Errorf("main.ContentType(): got %q, expected %q", got, "text/html")
	}

	data, err := GetMessageContentByType(ctx, message, "text/html")
	if err != nil {
		t.Fatalf("GetMessageContentByType: %+v", err)
	}

	if string(data) != "<p>hello</p>" {
		t.Errorf("GetMessageContentByType: got %q, expected %q", string(data), "<p>hello</p>")
	}
}

func TestMessageWithoutMainPart(t *testing.T) {
	message := NewMessage("msg-1", NewChannelRef("chan-1"), NewUser("user-1", "User"))

	if _, err := GetMessageMainPart(message); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetMessageMainPart: got error %v, expected ErrNotFound", err)
	}
}

func TestMentions(t *testing.T) {
	message := NewMessage(
		"msg-1",
		NewChannel("chan-1", ChannelKindGroup, "team"),
		NewUser("user-1", "User"),
		WithMessageMainPart("@bot hello"),
		WithMessageMentions(Mention{UserID: "bot", DisplayName: "Bot"}),
	)

	if !IsMentioned(message, "bot") {
		t.Error("IsMentioned(message, \"bot\"): got false, expected true")
	}

	if IsMentioned(message, "someone-else") {
		t.Error("IsMentioned(message, \"someone-else\"): got true, expected false")
	}

	if got := len(Mentions(message)); got != 1 {
		t.Errorf("len(Mentions(message)): got %d, expected 1", got)
	}
}

func TestInReplyTo(t *testing.T) {
	orphan := NewMessage("msg-1", NewChannelRef("chan-1"), NewUser("user-1", "User"))

	if _, ok := InReplyTo(orphan); ok {
		t.Error("InReplyTo(orphan): got ok, expected not ok")
	}

	reply := NewMessage(
		"msg-2",
		NewChannelRef("chan-1"),
		NewUser("user-1", "User"),
		WithMessageInReplyTo("msg-1"),
	)

	parent, ok := InReplyTo(reply)
	if !ok {
		t.Fatal("InReplyTo(reply): got not ok, expected ok")
	}

	if parent != "msg-1" {
		t.Errorf("InReplyTo(reply): got %q, expected %q", parent, "msg-1")
	}
}

func TestIsGroupChannel(t *testing.T) {
	for _, tc := range []struct {
		kind     ChannelKind
		expected bool
	}{
		{ChannelKindDirect, false},
		{ChannelKindGroup, true},
		{ChannelKindPublic, true},
		{ChannelKindUnknown, false},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			if got := IsGroupChannel(NewChannel("chan-1", tc.kind, "")); got != tc.expected {
				t.Errorf("IsGroupChannel(%q): got %v, expected %v", tc.kind, got, tc.expected)
			}
		})
	}

	if IsGroupChannel(nil) {
		t.Error("IsGroupChannel(nil): got true, expected false")
	}
}
