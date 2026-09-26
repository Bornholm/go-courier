package whatsapp

import (
	"context"
	"fmt"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"
)

// ConnectionState names what happened to the link between the client and
// WhatsApp's servers. The application cares about two things: is the
// account still usable, and if not, will it come back on its own.
type ConnectionState string

const (
	// ConnectionConnected: the socket is up and the account is logged in.
	ConnectionConnected ConnectionState = "connected"
	// ConnectionDisconnected: the socket dropped. whatsmeow reconnects by
	// itself; this is informative unless it keeps happening.
	ConnectionDisconnected ConnectionState = "disconnected"
	// ConnectionKeepAliveTimeout: the server stopped answering pings. The
	// TCP connection usually dies shortly after, then reconnects.
	ConnectionKeepAliveTimeout ConnectionState = "keepalive_timeout"
	// ConnectionLoggedOut: the device was unlinked — from the phone, by
	// WhatsApp after too long without contact with it, or by a 401 on
	// connect. The session is gone; only a new pairing brings it back.
	ConnectionLoggedOut ConnectionState = "logged_out"
	// ConnectionStreamReplaced: another client connected with the same
	// session. Two processes sharing one session file, typically. This
	// client will not reconnect.
	ConnectionStreamReplaced ConnectionState = "stream_replaced"
	// ConnectionClientOutdated: WhatsApp refuses this protocol version.
	// Upgrading whatsmeow is the only remedy.
	ConnectionClientOutdated ConnectionState = "client_outdated"
	// ConnectionTemporaryBan: the account is banned for a while.
	ConnectionTemporaryBan ConnectionState = "temporary_ban"
	// ConnectionFailed: the connect attempt was refused for another reason.
	ConnectionFailed ConnectionState = "connect_failure"
)

// ConnectionEvent is what a ConnectionHandler receives.
type ConnectionEvent struct {
	State ConnectionState
	// Permanent is true when whatsmeow will NOT reconnect on its own: the
	// application has to act — re-pair, upgrade, wait out a ban. It is the
	// bit that matters most, and it is computed here rather than left to
	// callers to derive from State.
	Permanent bool
	// Reason is a human-readable detail, empty when there is none.
	Reason string
}

// ConnectionHandler observes the connection's life. It runs on whatsmeow's
// event goroutine: keep it short, never block.
//
// Without a handler, a lost session is silent: the client stops receiving
// and nothing says why until the next restart shows a pairing code. That
// silence cost a two-day outage on 2026-09-23.
type ConnectionHandler func(ctx context.Context, event ConnectionEvent)

// connectionEventOf translates a whatsmeow event into a ConnectionEvent,
// or reports false for events that are not about the connection.
func connectionEventOf(evt any) (ConnectionEvent, bool) {
	switch e := evt.(type) {
	case *events.Connected:
		return ConnectionEvent{State: ConnectionConnected}, true
	case *events.Disconnected:
		return ConnectionEvent{State: ConnectionDisconnected}, true
	case *events.KeepAliveTimeout:
		return ConnectionEvent{
			State:  ConnectionKeepAliveTimeout,
			Reason: fmt.Sprintf("%d failed keepalives, last success %s", e.ErrorCount, e.LastSuccess.Format("15:04:05")),
		}, true
	case *events.KeepAliveRestored:
		return ConnectionEvent{State: ConnectionConnected, Reason: "keepalive restored"}, true
	case *events.LoggedOut:
		return ConnectionEvent{State: ConnectionLoggedOut, Permanent: true, Reason: e.PermanentDisconnectDescription()}, true
	case *events.StreamReplaced:
		return ConnectionEvent{State: ConnectionStreamReplaced, Permanent: true, Reason: e.PermanentDisconnectDescription()}, true
	case *events.ClientOutdated:
		return ConnectionEvent{State: ConnectionClientOutdated, Permanent: true, Reason: e.PermanentDisconnectDescription()}, true
	case *events.TemporaryBan:
		return ConnectionEvent{State: ConnectionTemporaryBan, Permanent: true, Reason: e.PermanentDisconnectDescription()}, true
	case *events.ConnectFailure:
		return ConnectionEvent{State: ConnectionFailed, Permanent: true, Reason: e.PermanentDisconnectDescription()}, true
	}
	return ConnectionEvent{}, false
}

// watchConnection wires the handler, if any, onto the client.
func (p *Provider) watchConnection(ctx context.Context, client *whatsmeow.Client) {
	if p.opts.ConnectionHandler == nil {
		return
	}
	client.AddEventHandler(func(evt any) {
		if event, ok := connectionEventOf(evt); ok {
			p.opts.ConnectionHandler(ctx, event)
		}
	})
}
