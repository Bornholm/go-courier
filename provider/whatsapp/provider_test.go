package whatsapp

import (
	"testing"

	"github.com/bornholm/go-courier"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func TestKindOf(t *testing.T) {
	for _, tc := range []struct {
		raw      string
		expected courier.ChannelKind
	}{
		{"33612345678@s.whatsapp.net", courier.ChannelKindDirect},
		{"1234567890@lid", courier.ChannelKindDirect},
		{"120363000000000000@g.us", courier.ChannelKindGroup},
		{"1234567890@newsletter", courier.ChannelKindPublic},
		{"1234567890@broadcast", courier.ChannelKindPublic},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			jid, err := types.ParseJID(tc.raw)
			if err != nil {
				t.Fatalf("types.ParseJID(%q): %+v", tc.raw, err)
			}

			if got := kindOf(jid); got != tc.expected {
				t.Errorf("kindOf(%q): got %q, expected %q", tc.raw, got, tc.expected)
			}
		})
	}
}

func TestTextOf(t *testing.T) {
	for _, tc := range []struct {
		name     string
		message  *waE2E.Message
		expected string
	}{
		{
			name:     "plain conversation",
			message:  &waE2E.Message{Conversation: proto.String("hello")},
			expected: "hello",
		},
		{
			name: "extended text",
			message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: proto.String("hello again"),
			}},
			expected: "hello again",
		},
		{
			name:     "media only message has no text",
			message:  &waE2E.Message{ImageMessage: &waE2E.ImageMessage{}},
			expected: "",
		},
		{
			name:     "nil message",
			message:  nil,
			expected: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := textOf(tc.message); got != tc.expected {
				t.Errorf("textOf: got %q, expected %q", got, tc.expected)
			}
		})
	}
}

func TestMediaOfVoiceNote(t *testing.T) {
	message := &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
		Mimetype:   proto.String("audio/ogg; codecs=opus"),
		Seconds:    proto.Uint32(4),
		PTT:        proto.Bool(true),
		FileLength: proto.Uint64(18244),
	}}

	media, ok := mediaOf(message)
	if !ok {
		t.Fatal("mediaOf: got not ok, expected ok")
	}

	if !media.voiceNote {
		t.Error("media.voiceNote: got false, expected true — PTT marks a voice note")
	}

	if got := media.duration.Seconds(); got != 4 {
		t.Errorf("media.duration: got %vs, expected 4s", got)
	}

	if got := courier.MediaKindOf(media.mimeType); got != courier.MediaKindAudio {
		t.Errorf("media kind: got %q, expected %q", got, courier.MediaKindAudio)
	}

	if got := media.size; got != 18244 {
		t.Errorf("media.size: got %d, expected 18244", got)
	}
}

func TestMediaOfSharedAudioIsNotAVoiceNote(t *testing.T) {
	message := &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
		Mimetype: proto.String("audio/mpeg"),
		Seconds:  proto.Uint32(180),
	}}

	media, ok := mediaOf(message)
	if !ok {
		t.Fatal("mediaOf: got not ok, expected ok")
	}

	if media.voiceNote {
		t.Error("media.voiceNote: got true, expected false — a shared audio file is not a voice note")
	}
}

func TestMediaOfDocument(t *testing.T) {
	message := &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
		Mimetype: proto.String("application/pdf"),
		FileName: proto.String("report.pdf"),
		Caption:  proto.String("last quarter"),
	}}

	media, ok := mediaOf(message)
	if !ok {
		t.Fatal("mediaOf: got not ok, expected ok")
	}

	if media.filename != "report.pdf" {
		t.Errorf("media.filename: got %q, expected %q", media.filename, "report.pdf")
	}

	if media.caption != "last quarter" {
		t.Errorf("media.caption: got %q, expected %q", media.caption, "last quarter")
	}
}

func TestMediaOfTextMessage(t *testing.T) {
	if _, ok := mediaOf(&waE2E.Message{Conversation: proto.String("hello")}); ok {
		t.Error("mediaOf: got ok, expected not ok for a text only message")
	}
}

func TestMediaTypeFor(t *testing.T) {
	// A voice note must go to the audio bucket, not the document one.
	if got := mediaTypeFor("audio/ogg; codecs=opus"); got != "WhatsApp Audio Keys" {
		t.Errorf("mediaTypeFor(audio): got %q, expected the audio bucket", got)
	}

	if got := mediaTypeFor("application/pdf"); got != "WhatsApp Document Keys" {
		t.Errorf("mediaTypeFor(pdf): got %q, expected the document bucket", got)
	}
}

// A message sent from a linked device carries the device in its JID
// ("<user>:22@lid"). Two messages from the same person must still yield the
// same user identity, whichever device they were sent from.
func TestUserIDOfStripsTheDevice(t *testing.T) {
	for _, test := range []struct {
		name     string
		jid      types.JID
		expected courier.UserID
	}{
		{
			name:     "linked device",
			jid:      types.JID{User: "175913320902842", Device: 22, Server: types.HiddenUserServer},
			expected: "175913320902842@lid",
		},
		{
			name:     "primary device",
			jid:      types.JID{User: "175913320902842", Server: types.HiddenUserServer},
			expected: "175913320902842@lid",
		},
		{
			name:     "phone number",
			jid:      types.JID{User: "33600000000", Device: 3, Server: types.DefaultUserServer},
			expected: "33600000000@s.whatsapp.net",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := userIDOf(test.jid); got != test.expected {
				t.Errorf("userIDOf(%s) = %q, expected %q", test.jid, got, test.expected)
			}
		})
	}
}
