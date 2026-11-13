package mail

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/bornholm/go-courier"
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// imapTestServer starts an in-memory IMAP server over TLS with a certificate
// signed for localhost, and returns its address and the matching client TLS
// configuration.
func imapTestServer(t *testing.T) (string, *tls.Config) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)

	memServer := imapmemserver.New()
	user := imapmemserver.NewUser("bot", "secret")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	memServer.AddUser(user)

	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		Caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}},
	})
	ln, err := tls.Listen("tcp", "localhost:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	})
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(ln)
	t.Cleanup(func() { server.Close() })

	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return "localhost:" + port, &tls.Config{RootCAs: roots}
}

// testClient : a direct connection to the test server, to append messages and
// read their flags.
func testClient(t *testing.T, address string, config *tls.Config) *imapclient.Client {
	t.Helper()
	client, err := imapclient.DialTLS(address, &imapclient.Options{TLSConfig: config})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Login("bot", "secret").Wait(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func appendEmail(t *testing.T, client *imapclient.Client, id string, flags ...imap.Flag) {
	t.Helper()
	raw := fmt.Sprintf("From: client@example.org\r\nTo: contact@example.org\r\nSubject: %s\r\n"+
		"Message-ID: <%s@example.org>\r\nDate: %s\r\nContent-Type: text/plain\r\n\r\nBonjour, %s.\r\n",
		id, id, time.Now().Format(time.RFC1123Z), id)
	cmd := client.Append("INBOX", int64(len(raw)), &imap.AppendOptions{Flags: flags})
	if _, err := cmd.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

// seenFlags returns the subjects of the INBOX messages carrying \Seen.
func seenFlags(t *testing.T, client *imapclient.Client) []string {
	t.Helper()
	if _, err := client.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatal(err)
	}
	msgs, err := client.Fetch(imap.SeqSetNum(1, 2, 3, 4), &imap.FetchOptions{Flags: true, Envelope: true}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, msg := range msgs {
		if slices.Contains(msg.Flags, imap.FlagSeen) {
			seen = append(seen, msg.Envelope.Subject)
		}
	}
	sort.Strings(seen)
	return seen
}

// receive collects message ids from the channel until n arrived or the timeout.
func receive(t *testing.T, messages chan courier.Message, n int, timeout time.Duration) []string {
	t.Helper()
	var ids []string
	deadline := time.After(timeout)
	for len(ids) < n {
		select {
		case m := <-messages:
			ids = append(ids, string(m.ID()))
		case <-deadline:
			sort.Strings(ids)
			return ids
		}
	}
	sort.Strings(ids)
	return ids
}

// WithIMAPReadOnly: an already read message is received too, no flag changes,
// and a later check only brings the new messages.
func TestReadOnly(t *testing.T) {
	address, config := imapTestServer(t)
	client := testClient(t, address, config)
	appendEmail(t, client, "un")
	appendEmail(t, client, "deux", imap.FlagSeen) // already read by a colleague
	appendEmail(t, client, "trois")

	provider := NewProvider(
		WithIMAP(address, "bot", "secret"),
		WithIMAPTLSConfig(config),
		WithIMAPReadOnly(time.Hour),
		WithIMAPCheckInterval(50*time.Millisecond),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	messages, err := provider.Listen(ctx)
	if err != nil {
		t.Fatal(err)
	}

	got := receive(t, messages, 3, 5*time.Second)
	want := []string{"deux@example.org", "trois@example.org", "un@example.org"}
	if !slices.Equal(got, want) {
		t.Fatalf("first check: got %v, want %v", got, want)
	}
	if seen := seenFlags(t, client); !slices.Equal(seen, []string{"deux"}) {
		t.Errorf("flags changed: messages seen %v, want only [deux]", seen)
	}

	appendEmail(t, client, "quatre")
	if got := receive(t, messages, 2, 2*time.Second); !slices.Equal(got, []string{"quatre@example.org"}) {
		t.Errorf("later checks: got %v, want only the new message", got)
	}
	if seen := seenFlags(t, client); !slices.Equal(seen, []string{"deux"}) {
		t.Errorf("flags changed after the new message: %v", seen)
	}
}

// By default (no WithIMAPReadOnly), the behaviour is unchanged: unread messages
// only, marked read once fetched.
func TestDefaultMarksRead(t *testing.T) {
	address, config := imapTestServer(t)
	client := testClient(t, address, config)
	appendEmail(t, client, "un")
	appendEmail(t, client, "deux", imap.FlagSeen)

	provider := NewProvider(WithIMAP(address, "bot", "secret"), WithIMAPTLSConfig(config),
		WithIMAPCheckInterval(50*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	messages, err := provider.Listen(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := receive(t, messages, 2, 2*time.Second); !slices.Equal(got, []string{"un@example.org"}) {
		t.Errorf("got %v, want only the unread message", got)
	}
	if seen := seenFlags(t, client); !slices.Equal(seen, []string{"deux", "un"}) {
		t.Errorf("messages seen %v, want [deux un]", seen)
	}
}

// The server certificate is verified by default: an unknown authority is refused.
func TestCertificateVerified(t *testing.T) {
	address, _ := imapTestServer(t)
	provider := NewProvider(WithIMAP(address, "bot", "secret"))
	if _, err := provider.getImapClient(); err == nil {
		t.Fatal("connection accepted with an unverified certificate")
	}
}
