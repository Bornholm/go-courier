package discord

import (
	"testing"

	"github.com/bornholm/go-courier"
	"github.com/bwmarrin/discordgo"
)

func TestKindOf(t *testing.T) {
	for _, tc := range []struct {
		name        string
		channelType discordgo.ChannelType
		expected    courier.ChannelKind
	}{
		{"dm", discordgo.ChannelTypeDM, courier.ChannelKindDirect},
		{"group dm", discordgo.ChannelTypeGroupDM, courier.ChannelKindGroup},
		{"private thread", discordgo.ChannelTypeGuildPrivateThread, courier.ChannelKindGroup},
		{"guild text", discordgo.ChannelTypeGuildText, courier.ChannelKindPublic},
		{"guild forum", discordgo.ChannelTypeGuildForum, courier.ChannelKindPublic},
		{"voice", discordgo.ChannelTypeGuildVoice, courier.ChannelKindUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := kindOf(tc.channelType); got != tc.expected {
				t.Errorf("kindOf(%v): got %q, expected %q", tc.channelType, got, tc.expected)
			}
		})
	}
}

func TestToAttachment(t *testing.T) {
	provider := NewProvider()
	defer provider.releaseAll()

	attachment := provider.toAttachment(0, &discordgo.MessageAttachment{
		URL:          "https://cdn.example.org/note.ogg",
		Filename:     "note.ogg",
		ContentType:  "audio/ogg",
		Size:         2048,
		DurationSecs: 4.5,
	})

	if got := attachment.Name(); got != "att-0" {
		t.Errorf("attachment.Name(): got %q, expected %q", got, "att-0")
	}

	if got := attachment.Size(); got != 2048 {
		t.Errorf("attachment.Size(): got %d, expected 2048", got)
	}

	// A duration is only reported for voice messages.
	if !courier.IsVoiceNote(attachment) {
		t.Error("courier.IsVoiceNote(attachment): got false, expected true")
	}

	voiceNote, ok := attachment.(courier.VoiceNote)
	if !ok {
		t.Fatal("attachment does not implement courier.VoiceNote")
	}

	if got := voiceNote.Duration().Milliseconds(); got != 4500 {
		t.Errorf("voiceNote.Duration(): got %dms, expected 4500ms", got)
	}
}

func TestToAttachmentWithoutContentType(t *testing.T) {
	provider := NewProvider()
	defer provider.releaseAll()

	attachment := provider.toAttachment(1, &discordgo.MessageAttachment{
		URL:      "https://cdn.example.org/blob",
		Filename: "blob",
	})

	if got := attachment.ContentType(); got != "application/octet-stream" {
		t.Errorf("attachment.ContentType(): got %q, expected %q", got, "application/octet-stream")
	}

	if courier.IsVoiceNote(attachment) {
		t.Error("courier.IsVoiceNote(attachment): got true, expected false")
	}
}
