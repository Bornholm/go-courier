package rest_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/couriertest"
	"github.com/bornholm/go-courier/provider/rest"
	"github.com/pkg/errors"
)

const testToken = "test-token"

// testServer starts a provider on a free port and collects everything the
// clients receive over SSE.
type testServer struct {
	provider *rest.Provider
	baseURL  string
	incoming chan courier.Message

	mutex  sync.Mutex
	events []rest.MessageDTO
}

func startTestServer(t *testing.T, funcs ...rest.OptionFunc) *testServer {
	t.Helper()

	address := freeAddress(t)

	opts := append([]rest.OptionFunc{
		rest.WithAddress(address),
		rest.WithTokens(map[string]courier.User{
			testToken: courier.NewUser("client-1", "Client"),
		}),
	}, funcs...)

	provider := rest.NewProvider(opts...)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	incoming, err := provider.Listen(ctx)
	if err != nil {
		t.Fatalf("provider.Listen(ctx): %+v", err)
	}

	server := &testServer{
		provider: provider,
		baseURL:  "http://" + address,
		incoming: incoming,
		events:   []rest.MessageDTO{},
	}

	server.waitReady(t)
	server.subscribe(t, ctx, "conformance")

	return server
}

// freeAddress reserves a port and releases it, so the server can bind it.
func freeAddress(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %+v", err)
	}

	address := listener.Addr().String()

	if err := listener.Close(); err != nil {
		t.Fatalf("listener.Close(): %+v", err)
	}

	return address
}

func (s *testServer) waitReady(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		resp, err := http.Get(s.baseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("server did not become ready within 5s")
}

// subscribe opens an SSE connection and records every event it receives.
func (s *testServer) subscribe(t *testing.T, ctx context.Context, channelID string) {
	t.Helper()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/channels/%s/events", s.baseURL, channelID), nil)
	if err != nil {
		t.Fatalf("http.NewRequestWithContext: %+v", err)
	}

	req.Header.Set("Authorization", "Bearer "+testToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http.DefaultClient.Do: %+v", err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("GET /channels/%s/events: got status %d, expected 200", channelID, resp.StatusCode)
	}

	ready := make(chan struct{})

	go func() {
		defer resp.Body.Close()

		close(ready)

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		for scanner.Scan() {
			payload, found := strings.CutPrefix(scanner.Text(), "data: ")
			if !found {
				continue
			}

			var dto rest.MessageDTO
			if err := json.Unmarshal([]byte(payload), &dto); err != nil {
				continue
			}

			s.mutex.Lock()
			s.events = append(s.events, dto)
			s.mutex.Unlock()
		}

		// The stream ends when the test context is cancelled, so a scan error
		// here is expected and not worth reporting.
		_ = scanner.Err()
	}()

	<-ready

	// Give the server a moment to register the subscriber before the first
	// publish, otherwise the event is fanned out to nobody.
	time.Sleep(50 * time.Millisecond)
}

// collected returns the events received so far, waiting up to timeout for at
// least one to show up. Publishing to a subscriber and reading it back over
// the network is asynchronous, so a caller checking right after Send would
// otherwise observe nothing.
func (s *testServer) collected(timeout time.Duration) []rest.MessageDTO {
	deadline := time.Now().Add(timeout)

	for {
		s.mutex.Lock()
		events := make([]rest.MessageDTO, len(s.events))
		copy(events, s.events)
		s.mutex.Unlock()

		if len(events) > 0 || time.Now().After(deadline) {
			return events
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// post sends a message through the HTTP API, mirroring what a real client
// would do.
func (s *testServer) post(ctx context.Context, channelID string, message courier.Message) error {
	content, err := courier.GetMessageMainContent(ctx, message)
	if err != nil {
		return errors.WithStack(err)
	}

	incoming := rest.IncomingMessageDTO{Content: content}

	if parent, ok := courier.InReplyTo(message); ok {
		incoming.InReplyTo = string(parent)
	}

	for _, m := range courier.Mentions(message) {
		incoming.Mentions = append(incoming.Mentions, rest.MentionDTO{
			UserID:      string(m.UserID),
			DisplayName: m.DisplayName,
		})
	}

	payload, err := json.Marshal(incoming)
	if err != nil {
		return errors.WithStack(err)
	}

	var body bytes.Buffer

	writer := multipart.NewWriter(&body)

	if err := writer.WriteField("message", string(payload)); err != nil {
		return errors.WithStack(err)
	}

	for _, attachment := range courier.Attachments(message) {
		data, err := courier.ReadPart(ctx, attachment)
		if err != nil {
			return errors.WithStack(err)
		}

		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", fmt.Sprintf(
			`form-data; name="files"; filename=%q`, courier.FilenameFor(attachment),
		))
		header.Set("Content-Type", attachment.ContentType())

		part, err := writer.CreatePart(header)
		if err != nil {
			return errors.WithStack(err)
		}

		if _, err := part.Write(data); err != nil {
			return errors.WithStack(err)
		}
	}

	if err := writer.Close(); err != nil {
		return errors.WithStack(err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/channels/%s/messages", s.baseURL, channelID), &body)
	if err != nil {
		return errors.WithStack(err)
	}

	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return errors.WithStack(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return errors.Errorf("POST messages: got status %d, expected 201", resp.StatusCode)
	}

	return nil
}

func (s *testServer) get(t *testing.T, path string) (*http.Response, []byte) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, s.baseURL+path, nil)
	if err != nil {
		t.Fatalf("http.NewRequest: %+v", err)
	}

	req.Header.Set("Authorization", "Bearer "+testToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http.DefaultClient.Do: %+v", err)
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("io.ReadAll: %+v", err)
	}

	return resp, body
}

func TestProviderConformance(t *testing.T) {
	couriertest.RunProviderSuite(t, func(t *testing.T) *couriertest.Harness {
		server := startTestServer(t, rest.WithChannelKind(courier.ChannelKindGroup))

		return &couriertest.Harness{
			Provider: server.provider,
			Channel:  courier.NewChannel("conformance", courier.ChannelKindGroup, "conformance"),
			From:     courier.NewUser("client-1", "Client"),

			// The provider owns a server socket and accepts a single Listen,
			// already called when the test server started.
			Listen: func(ctx context.Context) (chan courier.Message, error) {
				return server.incoming, nil
			},

			// Incoming messages travel through the real HTTP endpoint, then
			// come back on the provider channel.
			Deliver: func(ctx context.Context, message courier.Message) error {
				if err := server.post(ctx, "conformance", message); err != nil {
					return errors.WithStack(err)
				}

				return nil
			},

			// Outgoing messages are observed through the SSE stream, decoded
			// back into courier messages.
			Sent: func() []courier.Message {
				events := server.collected(2 * time.Second)
				messages := make([]courier.Message, 0, len(events))

				for _, event := range events {
					messages = append(messages, fromDTO(event))
				}

				return messages
			},
		}
	})
}

// fromDTO rebuilds a courier.Message from an SSE payload, so the conformance
// suite can assert on what clients actually received.
func fromDTO(dto rest.MessageDTO) courier.Message {
	funcs := []courier.BaseMessageOptionFunc{
		courier.WithMessageSentAt(dto.SentAt),
	}

	if dto.InReplyTo != "" {
		funcs = append(funcs, courier.WithMessageInReplyTo(courier.MessageID(dto.InReplyTo)))
	}

	for _, m := range dto.Mentions {
		funcs = append(funcs, courier.WithMessageMentions(courier.Mention{
			UserID:      courier.UserID(m.UserID),
			DisplayName: m.DisplayName,
		}))
	}

	for _, part := range dto.Parts {
		if part.Name == courier.PartMain {
			funcs = append(funcs, courier.WithMessageMainPartOfType(part.Content, part.ContentType))
			continue
		}

		funcs = append(funcs, courier.WithMessagePart(courier.NewAttachment(
			part.Filename, part.ContentType, courier.OpenerFromString(part.Content),
			courier.WithAttachmentName(part.Name),
		)))
	}

	return courier.NewMessage(
		courier.MessageID(dto.ID),
		courier.NewChannel(courier.ChannelID(dto.Channel.ID), courier.ChannelKind(dto.Channel.Kind), dto.Channel.Name),
		courier.NewUser(courier.UserID(dto.From.ID), dto.From.DisplayName),
		funcs...,
	)
}

func TestUnauthorized(t *testing.T) {
	server := startTestServer(t)

	resp, err := http.Get(server.baseURL + "/channels/demo/events")
	if err != nil {
		t.Fatalf("http.Get: %+v", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET without token: got status %d, expected 401", resp.StatusCode)
	}
}

func TestAttachmentDownloadIsReplayable(t *testing.T) {
	ctx := t.Context()

	server := startTestServer(t)

	message := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannel("conformance", courier.ChannelKindDirect, "conformance"),
		courier.NewUser("client-1", "Client"),
		courier.WithMessageMainPart("listen to this"),
		courier.WithMessagePart(courier.NewAttachment(
			"note.ogg", "audio/ogg", courier.OpenerFromString("fake audio"),
			courier.WithAttachmentVoiceNote(4*time.Second),
		)),
	)

	if err := server.post(ctx, "conformance", message); err != nil {
		t.Fatalf("server.post: %+v", err)
	}

	received := <-server.incoming

	attachments := courier.Attachments(received)
	if len(attachments) != 1 {
		t.Fatalf("len(courier.Attachments(received)): got %d, expected 1", len(attachments))
	}

	// The uploaded file is downloadable through the API, twice, since the
	// upload was buffered rather than streamed straight from the request.
	path := fmt.Sprintf("/messages/%s/parts/%s", received.ID(), attachments[0].Name())

	for attempt := 1; attempt <= 2; attempt++ {
		resp, body := server.get(t, path)

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("attempt %d: got status %d, expected 200", attempt, resp.StatusCode)
		}

		if got := resp.Header.Get("Content-Type"); got != "audio/ogg" {
			t.Errorf("attempt %d Content-Type: got %q, expected %q", attempt, got, "audio/ogg")
		}

		if string(body) != "fake audio" {
			t.Errorf("attempt %d body: got %q, expected %q", attempt, string(body), "fake audio")
		}
	}
}

func TestUploadTooLarge(t *testing.T) {
	ctx := t.Context()

	server := startTestServer(t, rest.WithMaxUploadSize(8))

	message := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef("conformance"),
		courier.NewUser("client-1", "Client"),
		courier.WithMessageMainPart("too big"),
		courier.WithMessagePart(courier.NewAttachment(
			"big.bin", "application/octet-stream",
			courier.OpenerFromString(strings.Repeat("x", 64)),
		)),
	)

	if err := server.post(ctx, "conformance", message); err == nil {
		t.Fatal("server.post: got no error, expected the upload to be rejected")
	}
}

func TestSendBeforeListen(t *testing.T) {
	provider := rest.NewProvider()

	err := provider.Send(context.Background(), courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef("demo"),
		courier.NewUser("user-1", "User"),
		courier.WithMessageMainPart("hello"),
	))

	if err == nil {
		t.Error("provider.Send before Listen: got no error, expected one")
	}
}
