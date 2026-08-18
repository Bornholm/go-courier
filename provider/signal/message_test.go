package signal

import (
	"encoding/json"
	"testing"

	"github.com/bornholm/go-courier"
)

// Les enveloppes sans contenu utilisateur (reçus, frappes, synchronisation)
// ne deviennent jamais des messages : l'application ne doit pas être
// réveillée pour un accusé de réception.
func TestToMessage_IgnoresNonContentEnvelopes(t *testing.T) {
	provider := NewProvider()

	for name, raw := range map[string]string{
		"receipt": `{"envelope":{"source":"+337","timestamp":1,"receiptMessage":{"isDelivery":true}}}`,
		"typing":  `{"envelope":{"source":"+337","timestamp":1,"typingMessage":{"action":"STARTED"}}}`,
		"empty":   `{"envelope":{"source":"+337","timestamp":1,"dataMessage":{"message":""}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := provider.toMessage(json.RawMessage(raw)); ok {
				t.Error("cette enveloppe ne doit produire aucun message")
			}
		})
	}
}

// Un message de groupe est rattaché au groupe (préfixé), un message direct
// au numéro de l'expéditeur : c'est ce qui fait qu'une réponse repart au
// bon endroit.
func TestToMessage_Channels(t *testing.T) {
	provider := NewProvider()

	direct, ok := provider.toMessage(json.RawMessage(`{"envelope":{"source":"+33712345678","sourceName":"Alice","timestamp":1712345,"dataMessage":{"message":"coucou","timestamp":1712345}}}`))
	if !ok {
		t.Fatal("message direct attendu")
	}
	if direct.Channel().ChannelID() != "+33712345678" || direct.Channel().Kind() != courier.ChannelKindDirect {
		t.Errorf("canal direct = (%q, %q)", direct.Channel().ChannelID(), direct.Channel().Kind())
	}
	if direct.ID() != "+33712345678:1712345" {
		t.Errorf("id = %q, attendu source:timestamp", direct.ID())
	}

	group, ok := provider.toMessage(json.RawMessage(`{"envelope":{"source":"+33712345678","timestamp":1,"dataMessage":{"message":"coucou","groupInfo":{"groupId":"Zm9v"}}}}`))
	if !ok {
		t.Fatal("message de groupe attendu")
	}
	if group.Channel().ChannelID() != courier.ChannelID(groupChannelPrefix+"Zm9v") || group.Channel().Kind() != courier.ChannelKindGroup {
		t.Errorf("canal groupe = (%q, %q)", group.Channel().ChannelID(), group.Channel().Kind())
	}
}

// Le quote d'un vrai daemon porte un id numérique (timestamp) : l'InReplyTo
// reconstruit doit avoir la même forme que les IDs entrants, sinon
// l'application ne retrouvera jamais le message cité.
func TestToMessage_QuoteMatchesIncomingIDShape(t *testing.T) {
	provider := NewProvider()

	message, ok := provider.toMessage(json.RawMessage(`{"envelope":{"source":"+337","timestamp":2,"dataMessage":{"message":"réponse","quote":{"id":1712345,"author":"+33698765432"}}}}`))
	if !ok {
		t.Fatal("message attendu")
	}

	parent, ok := courier.InReplyTo(message)
	if !ok {
		t.Fatal("InReplyTo attendu")
	}
	if parent != "+33698765432:1712345" {
		t.Errorf("parent = %q, attendu la forme source:timestamp", parent)
	}
}

// Une note vocale Signal est signalée comme telle : c'est ce qui déclenche
// la transcription côté application au lieu du traitement de pièce jointe.
func TestToMessage_VoiceNote(t *testing.T) {
	provider := NewProvider()

	message, ok := provider.toMessage(json.RawMessage(`{"envelope":{"source":"+337","timestamp":1,"dataMessage":{"attachments":[{"id":"a1","contentType":"audio/aac","voiceNote":true}]}}}`))
	if !ok {
		t.Fatal("message attendu")
	}

	attachments := courier.Attachments(message)
	if len(attachments) != 1 {
		t.Fatalf("attachments = %d", len(attachments))
	}
	if !courier.IsVoiceNote(attachments[0]) {
		t.Error("la note vocale doit être signalée comme telle")
	}
}
