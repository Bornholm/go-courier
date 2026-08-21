package whatsapp

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func incoming(chat types.JID, expiration uint32, ephemeral bool) *events.Message {
	event := &events.Message{IsEphemeral: ephemeral}
	event.Info.MessageSource.Chat = chat
	event.Message = &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text:        proto.String("hello"),
			ContextInfo: &waE2E.ContextInfo{},
		},
	}

	if expiration > 0 {
		event.Message.ExtendedTextMessage.ContextInfo.Expiration = proto.Uint32(expiration)
	}

	return event
}

func textPayload() *waE2E.Message {
	return &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("hi")},
	}
}

// A bot must not turn an ordinary conversation into a disappearing one.
func TestApplyExpirationLeavesAPlainChatAlone(t *testing.T) {
	provider := &Provider{opts: NewOptions()}
	chat := types.NewJID("33612345678", types.DefaultUserServer)

	payload := textPayload()
	provider.applyExpiration(payload, chat)

	if got := payload.GetExtendedTextMessage().GetContextInfo().GetExpiration(); got != 0 {
		t.Errorf("expiration: got %d, expected 0", got)
	}
}

func TestApplyExpirationMirrorsTheChat(t *testing.T) {
	provider := &Provider{opts: NewOptions()}
	chat := types.NewJID("120363000000000000", types.GroupServer)

	provider.rememberExpiration(incoming(chat, 604800, true))

	payload := textPayload()
	provider.applyExpiration(payload, chat)

	if got := payload.GetExtendedTextMessage().GetContextInfo().GetExpiration(); got != 604800 {
		t.Errorf("expiration: got %d, expected 604800", got)
	}
}

// Turning the timer off in a chat must stop the expiring replies.
func TestRememberExpirationForgetsADisabledTimer(t *testing.T) {
	provider := &Provider{opts: NewOptions()}
	chat := types.NewJID("33612345678", types.DefaultUserServer)

	provider.rememberExpiration(incoming(chat, 86400, true))
	provider.rememberExpiration(incoming(chat, 0, false))

	payload := textPayload()
	provider.applyExpiration(payload, chat)

	if got := payload.GetExtendedTextMessage().GetContextInfo().GetExpiration(); got != 0 {
		t.Errorf("expiration: got %d, expected 0", got)
	}
}

// An ephemeral message whose context info says nothing must not be read as a
// chat with the timer turned off.
func TestRememberExpirationKeepsAnEphemeralChat(t *testing.T) {
	provider := &Provider{opts: NewOptions()}
	chat := types.NewJID("33612345678", types.DefaultUserServer)

	provider.rememberExpiration(incoming(chat, 86400, true))
	provider.rememberExpiration(incoming(chat, 0, true))

	payload := textPayload()
	provider.applyExpiration(payload, chat)

	if got := payload.GetExtendedTextMessage().GetContextInfo().GetExpiration(); got != 86400 {
		t.Errorf("expiration: got %d, expected 86400", got)
	}
}

func TestApplyExpirationFallsBackToTheOption(t *testing.T) {
	provider := &Provider{opts: NewOptions(WithDisappearingTimer(24 * time.Hour))}
	chat := types.NewJID("33612345678", types.DefaultUserServer)

	payload := textPayload()
	provider.applyExpiration(payload, chat)

	if got := payload.GetExtendedTextMessage().GetContextInfo().GetExpiration(); got != 86400 {
		t.Errorf("expiration: got %d, expected 86400", got)
	}
}

// Media carried the setting of no chat at all before: only text did.
func TestApplyExpirationCoversMedia(t *testing.T) {
	provider := &Provider{opts: NewOptions()}
	chat := types.NewJID("33612345678", types.DefaultUserServer)

	provider.rememberExpiration(incoming(chat, 86400, true))

	payload := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{}}
	provider.applyExpiration(payload, chat)

	if got := payload.GetImageMessage().GetContextInfo().GetExpiration(); got != 86400 {
		t.Errorf("expiration: got %d, expected 86400", got)
	}
}
