package rocket

import (
	"net/url"
	"testing"
	"time"

	"github.com/gopackage/ddp"
)

// newStatusProvider builds a provider with just enough state for the status
// callback: it never dials anything.
func newStatusProvider() *Provider {
	serverURL, _ := url.Parse("https://rocket.example.test")

	return &Provider{
		opts: &Options{
			ServerURL: serverURL,
			Username:  "automata",
			Password:  "secret",
		},
	}
}

// The status callback is invoked by the DDP library from inside its own
// connection handling, and Close() ends by re-posting DISCONNECTED. Any work
// done under clientMutex therefore re-enters this function with the lock
// already held, and sync.Mutex is not reentrant: the provider used to
// deadlock for good, silently, exactly when a network outage hit.
//
// This test pins the property that matters: the callback returns, whatever
// it is handed, and holds no lock afterwards.
func TestStatus_NeverBlocks(t *testing.T) {
	provider := newStatusProvider()

	done := make(chan struct{})
	go func() {
		defer close(done)

		// The full sequence a failing reconnect produces.
		for _, status := range []int{
			ddp.DISCONNECTED,
			ddp.RECONNECTING,
			ddp.DISCONNECTING,
			ddp.DISCONNECTED,
			ddp.DIALING,
			ddp.CONNECTING,
		} {
			provider.Status(status)
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Status s'est bloqué : l'interblocage du callback est de retour")
	}

	// The lock must be free: a caller of Send or getClient would otherwise
	// hang forever, and the account would look connected while being mute.
	locked := make(chan struct{})
	go func() {
		provider.clientMutex.Lock()
		provider.clientMutex.Unlock()
		close(locked)
	}()

	select {
	case <-locked:
	case <-time.After(2 * time.Second):
		t.Fatal("clientMutex est resté verrouillé par le callback de statut")
	}
}

// Losing the connection arms a fresh login: a resumed DDP session does not
// carry Rocket.Chat's authentication, and without it the replayed
// subscription is accepted but stays mute.
func TestStatus_DisconnectArmsReauthentication(t *testing.T) {
	provider := newStatusProvider()

	if provider.needsLogin.Load() {
		t.Fatal("un provider neuf ne doit pas demander de ré-authentification")
	}

	provider.Status(ddp.DISCONNECTED)

	if !provider.needsLogin.Load() {
		t.Error("une déconnexion doit armer la ré-authentification")
	}
}

// The very first CONNECTED must not trigger a login: getClient has just done
// one, and a second would be a wasted round trip on every startup.
func TestStatus_FirstConnectionDoesNotReauthenticate(t *testing.T) {
	provider := newStatusProvider()

	// client is nil here: were reauthenticate to run, it would return at
	// once — but needsLogin tells us whether the path was even taken.
	provider.Status(ddp.CONNECTED)

	if provider.needsLogin.Load() {
		t.Error("la première connexion ne doit pas armer de ré-authentification")
	}
}
