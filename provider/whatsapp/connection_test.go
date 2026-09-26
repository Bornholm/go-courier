package whatsapp

import (
	"context"
	"testing"

	"go.mau.fi/whatsmeow/types/events"
)

// A lost session used to be silent: the client stopped receiving and
// nothing said why until the next restart showed a pairing code. The
// translation below is what lets an application tell a blip from a death.
func TestConnectionEventOf_TellsPermanentFromTransient(t *testing.T) {
	cases := []struct {
		evt       any
		state     ConnectionState
		permanent bool
	}{
		{&events.Connected{}, ConnectionConnected, false},
		{&events.Disconnected{}, ConnectionDisconnected, false},
		{&events.KeepAliveTimeout{ErrorCount: 3}, ConnectionKeepAliveTimeout, false},
		{&events.KeepAliveRestored{}, ConnectionConnected, false},
		{&events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}, ConnectionLoggedOut, true},
		{&events.StreamReplaced{}, ConnectionStreamReplaced, true},
		{&events.ClientOutdated{}, ConnectionClientOutdated, true},
		{&events.TemporaryBan{}, ConnectionTemporaryBan, true},
		{&events.ConnectFailure{Reason: events.ConnectFailureGeneric}, ConnectionFailed, true},
	}
	for _, tc := range cases {
		got, ok := connectionEventOf(tc.evt)
		if !ok {
			t.Fatalf("%T: événement non reconnu", tc.evt)
		}
		if got.State != tc.state || got.Permanent != tc.permanent {
			t.Errorf("%T: état %q permanent %v, attendu %q %v", tc.evt, got.State, got.Permanent, tc.state, tc.permanent)
		}
	}

	if _, ok := connectionEventOf(&events.Message{}); ok {
		t.Error("un message n'est pas un événement de connexion")
	}
	if got, _ := connectionEventOf(&events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}); got.Reason == "" {
		t.Error("une déconnexion définitive doit porter sa raison")
	}
}

func TestOptions_ConnectionHandlerIsConfigurable(t *testing.T) {
	called := false
	opts := NewOptions(WithConnectionHandler(func(context.Context, ConnectionEvent) { called = true }))
	if opts.ConnectionHandler == nil {
		t.Fatal("le gestionnaire n'a pas été enregistré")
	}
	opts.ConnectionHandler(context.Background(), ConnectionEvent{})
	if !called {
		t.Error("le gestionnaire enregistré n'a pas été appelé")
	}
	if NewOptions().ConnectionHandler != nil {
		t.Error("sans option, aucun gestionnaire ne doit être posé")
	}
}
