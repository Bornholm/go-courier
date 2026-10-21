package whatsappcloud

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/couriertest"
	"github.com/pkg/errors"
)

const (
	testPhoneNumberID = "1000"
	testAppSecret     = "app-secret"
	testVerifyToken   = "verify-token"
	testBusiness      = "+33 6 00 00 00 99"
)

// fakeGraph stands in for the Graph API.
type fakeGraph struct {
	t      *testing.T
	server *httptest.Server

	mu    sync.Mutex
	sent  []outgoing
	media map[string]fakeMedia
	next  int

	// refuse, when set, answers a message send with a Graph API error.
	refuse func(payload outgoing) *graphErrorBody
}

type fakeMedia struct {
	contentType string
	filename    string
	content     []byte
}

func newFakeGraph(t *testing.T) *fakeGraph {
	g := &fakeGraph{t: t, media: map[string]fakeMedia{}}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v24.0/"+testPhoneNumberID+"/messages", g.handleMessages)
	mux.HandleFunc("POST /v24.0/"+testPhoneNumberID+"/media", g.handleUpload)
	mux.HandleFunc("GET /v24.0/{id}", g.handleGet)
	mux.HandleFunc("GET /download/{id}", g.handleDownload)

	g.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(g.server.Close)

	return g
}

func (g *fakeGraph) handleMessages(w http.ResponseWriter, r *http.Request) {
	var payload outgoing
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if payload.Status == "read" {
		g.mu.Lock()
		g.sent = append(g.sent, payload)
		g.mu.Unlock()
		_, _ = io.WriteString(w, `{"success":true}`)
		return
	}

	if g.refuse != nil {
		if refusal := g.refuse(payload); refusal != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(refusal)
			return
		}
	}

	g.mu.Lock()
	g.next++
	id := fmt.Sprintf("wamid.%d", g.next)
	g.sent = append(g.sent, payload)
	g.mu.Unlock()

	fmt.Fprintf(w, `{"messaging_product":"whatsapp","messages":[{"id":%q}]}`, id)
}

func (g *fakeGraph) handleUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()

	content, _ := io.ReadAll(file)

	id := g.store(r.FormValue("type"), header.Filename, content)

	fmt.Fprintf(w, `{"id":%q}`, id)
}

func (g *fakeGraph) store(contentType, filename string, content []byte) string {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.next++
	id := "media-" + strconv.Itoa(g.next)
	g.media[id] = fakeMedia{contentType: contentType, filename: filename, content: content}

	return id
}

func (g *fakeGraph) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	switch {
	case id == testPhoneNumberID:
		fmt.Fprintf(w, `{"display_phone_number":%q,"verified_name":"Automata"}`, testBusiness)
		return
	case strings.HasPrefix(id, "grp"):
		fmt.Fprintf(w, `{"subject":"Famille"}`)
		return
	}

	g.mu.Lock()
	media, found := g.media[id]
	g.mu.Unlock()

	if !found {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"unknown media","code":100}}`)
		return
	}

	fmt.Fprintf(w, `{"url":%q,"mime_type":%q,"file_size":%d,"id":%q}`,
		g.server.URL+"/download/"+id, media.contentType, len(media.content), id)
}

func (g *fakeGraph) handleDownload(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	media, found := g.media[r.PathValue("id")]
	g.mu.Unlock()

	if !found {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", media.contentType)
	_, _ = w.Write(media.content)
}

func (g *fakeGraph) messages() []outgoing {
	g.mu.Lock()
	defer g.mu.Unlock()

	out := make([]outgoing, 0, len(g.sent))
	for _, payload := range g.sent {
		if payload.Status == "" {
			out = append(out, payload)
		}
	}

	return out
}

func newTestProvider(t *testing.T, graph *fakeGraph, funcs ...OptionFunc) *Provider {
	t.Helper()

	return NewProvider(append([]OptionFunc{
		WithCredentials("token", testPhoneNumberID),
		WithWebhookSecrets(testAppSecret, testVerifyToken),
		WithBaseURL(graph.server.URL),
		WithHTTPClient(graph.server.Client()),
	}, funcs...)...)
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// notify posts a signed webhook notification carrying messages and statuses.
func notify(t *testing.T, provider *Provider, contacts []webhookContact, messages []webhookMessage, statuses []webhookStatus) error {
	t.Helper()

	var value webhookValue
	value.MessagingProduct = "whatsapp"
	value.Metadata.PhoneNumberID = testPhoneNumberID
	value.Contacts = contacts
	value.Messages = messages
	value.Statuses = statuses

	body, err := json.Marshal(webhookPayload{
		Object: "whatsapp_business_account",
		Entry:  []webhookEntry{{ID: "waba", Changes: []webhookChange{{Field: "messages", Value: value}}}},
	})
	if err != nil {
		return errors.WithStack(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign(testAppSecret, body))

	rec := httptest.NewRecorder()
	provider.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		return errors.Errorf("webhook answered %d", rec.Code)
	}

	return nil
}

func textMessage(id, from, body string) webhookMessage {
	raw := webhookMessage{
		ID: id, From: from, Type: "text",
		Timestamp: strconv.FormatInt(time.Now().Unix(), 10),
	}
	raw.Text = &struct {
		Body string `json:"body"`
	}{Body: body}
	return raw
}

func receive(t *testing.T, messages chan courier.Message) courier.Message {
	t.Helper()

	select {
	case message := <-messages:
		return message
	case <-time.After(5 * time.Second):
		t.Fatal("no message received")
		return nil
	}
}

func TestProviderSuite(t *testing.T) {
	couriertest.RunProviderSuite(t, func(t *testing.T) *couriertest.Harness {
		graph := newFakeGraph(t)
		provider := newTestProvider(t, graph)

		user := courier.NewUser("33600000001", "Alice")

		var listenOnce sync.Once

		return &couriertest.Harness{
			Provider: provider,
			Channel:  courier.NewChannel("33600000001", courier.ChannelKindDirect, "Alice"),
			From:     user,
			Deliver: func(ctx context.Context, message courier.Message) error {
				text, err := courier.GetMessageMainContent(ctx, message)
				if err != nil && !errors.Is(err, courier.ErrNotFound) {
					return errors.WithStack(err)
				}

				for _, mention := range courier.Mentions(message) {
					text += " @" + string(mention.UserID)
				}

				raw := textMessage(string(message.ID()), string(user.ID()), text)

				// Every attachment travels as a document, the only media
				// type carrying a filename.
				for _, attachment := range courier.Attachments(message) {
					content, err := courier.ReadPart(ctx, attachment)
					if err != nil {
						return errors.WithStack(err)
					}

					id := graph.store(attachment.ContentType(), attachment.Filename(), content)

					raw.Type, raw.Text = "document", nil
					raw.Document = &webhookMedia{ID: id, MimeType: attachment.ContentType(), Filename: attachment.Filename(), Caption: text}
				}

				if parent, ok := courier.InReplyTo(message); ok {
					raw.Context = &struct {
						From string `json:"from"`
						ID   string `json:"id"`
					}{ID: string(parent)}
				}

				return notify(t, provider, []webhookContact{{WaID: string(user.ID())}}, []webhookMessage{raw}, nil)
			},
			Sent: func() []courier.Message {
				var sent []courier.Message
				for _, payload := range graph.messages() {
					funcs := []courier.BaseMessageOptionFunc{}
					if payload.Text != nil {
						funcs = append(funcs, courier.WithMessageMainPart(payload.Text.Body))
					}
					if payload.Document != nil {
						graph.mu.Lock()
						media := graph.media[payload.Document.ID]
						graph.mu.Unlock()

						funcs = append(funcs,
							courier.WithMessageMainPart(payload.Document.Caption),
							courier.WithMessagePart(courier.NewAttachment(payload.Document.Filename, media.contentType, courier.OpenerFromBytes(media.content))),
						)
					}
					sent = append(sent, courier.NewMessage(courier.RandomMessageID(), courier.NewChannelRef(courier.ChannelID(payload.To)), user, funcs...))
				}
				return sent
			},
			React: func(ctx context.Context, reaction courier.Reaction) error {
				// Reactions travel through the webhook, which only runs
				// while the provider listens.
				var err error
				listenOnce.Do(func() {
					var messages chan courier.Message
					if messages, err = provider.Listen(ctx); err == nil {
						go func() {
							for range messages {
							}
						}()
					}
				})
				if err != nil {
					return errors.WithStack(err)
				}

				raw := webhookMessage{
					ID: string(courier.RandomMessageID()), From: string(user.ID()), Type: "reaction",
					Timestamp: strconv.FormatInt(time.Now().Unix(), 10),
				}
				raw.Reaction = &struct {
					MessageID string `json:"message_id"`
					Emoji     string `json:"emoji"`
				}{MessageID: string(reaction.MessageID), Emoji: reaction.Emoji}

				return notify(t, provider, nil, []webhookMessage{raw}, nil)
			},
		}
	})
}

func TestWebhookVerification(t *testing.T) {
	provider := newTestProvider(t, newFakeGraph(t))

	for name, tc := range map[string]struct {
		query string
		code  int
	}{
		"valid":       {"hub.mode=subscribe&hub.verify_token=verify-token&hub.challenge=42", http.StatusOK},
		"wrong token": {"hub.mode=subscribe&hub.verify_token=nope&hub.challenge=42", http.StatusForbidden},
		"no mode":     {"hub.verify_token=verify-token&hub.challenge=42", http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			provider.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/webhook?"+tc.query, nil))

			if rec.Code != tc.code {
				t.Fatalf("status: got %d, expected %d", rec.Code, tc.code)
			}
			if tc.code == http.StatusOK && rec.Body.String() != "42" {
				t.Errorf("challenge: got %q, expected %q", rec.Body.String(), "42")
			}
		})
	}
}

func TestWebhookRefusesUnsignedNotifications(t *testing.T) {
	graph := newFakeGraph(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	post := func(provider *Provider, signature string) int {
		req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{"object":"whatsapp_business_account"}`))
		if signature != "" {
			req.Header.Set("X-Hub-Signature-256", signature)
		}
		rec := httptest.NewRecorder()
		provider.ServeHTTP(rec, req)
		return rec.Code
	}

	provider := newTestProvider(t, graph)
	if _, err := provider.Listen(ctx); err != nil {
		t.Fatal(err)
	}

	if code := post(provider, ""); code != http.StatusUnauthorized {
		t.Errorf("unsigned: got %d, expected %d", code, http.StatusUnauthorized)
	}
	if code := post(provider, sign("other-secret", []byte(`{"object":"whatsapp_business_account"}`))); code != http.StatusUnauthorized {
		t.Errorf("wrong secret: got %d, expected %d", code, http.StatusUnauthorized)
	}

	// Without an app secret, nothing is accepted, signed or not.
	open := newTestProvider(t, graph, WithWebhookSecrets("", testVerifyToken))
	if code := post(open, sign("", []byte(`{"object":"whatsapp_business_account"}`))); code != http.StatusForbidden {
		t.Errorf("no secret configured: got %d, expected %d", code, http.StatusForbidden)
	}
}

func TestWebhookIgnoresDuplicates(t *testing.T) {
	graph := newFakeGraph(t)
	provider := newTestProvider(t, graph)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	messages, err := provider.Listen(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if err := notify(t, provider, nil, []webhookMessage{textMessage("wamid.in", "33600000001", "hello")}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := notify(t, provider, nil, []webhookMessage{textMessage("wamid.in2", "33600000001", "again")}, nil); err != nil {
		t.Fatal(err)
	}

	if got := receive(t, messages).ID(); got != "wamid.in" {
		t.Fatalf("first message: got %q", got)
	}
	if got := receive(t, messages).ID(); got != "wamid.in2" {
		t.Fatalf("second message: got %q, expected the duplicate to be skipped", got)
	}
}

// reengage answers with a template carrying the preview.
func reengage(calls *int) ReengageFunc {
	return func(_ context.Context, _ courier.ChannelID, preview string) (Template, bool) {
		*calls++
		return Template{Name: "pending_message", Language: "fr", BodyParameters: []string{preview}}, true
	}
}

func TestSendHoldsBackPastTheServiceWindow(t *testing.T) {
	graph := newFakeGraph(t)

	windows := NewMemoryWindowStore()
	user := courier.UserID("33600000001")
	_ = windows.RecordInbound(context.Background(), user, time.Now().Add(-25*time.Hour))

	var calls int
	provider := newTestProvider(t, graph, WithWindowStore(windows), WithReengage(reengage(&calls)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	messages, err := provider.Listen(ctx)
	if err != nil {
		t.Fatal(err)
	}

	channel := courier.NewChannelRef(courier.ChannelID(user))

	for _, text := range []string{"Rappel :\n\tarroser   les      plantes", "Second rappel"} {
		if _, err := provider.Send(ctx, courier.NewMessage(courier.RandomMessageID(), channel, nil, courier.WithMessageMainPart(text))); err != nil {
			t.Fatalf("Send(%q): %+v", text, err)
		}
	}

	sent := graph.messages()
	if len(sent) != 1 || sent[0].Type != "template" {
		t.Fatalf("sent: got %+v, expected a single template", sent)
	}
	if got := sent[0].Template.Components[0].Parameters[0].Text; got != "Rappel : arroser les plantes" {
		t.Errorf("template parameter: got %q, expected the flattened preview", got)
	}
	if calls != 2 {
		t.Errorf("reengage calls: got %d, expected 2", calls)
	}

	// The user writes back: what was held back leaves first, then their
	// message reaches the application.
	if err := notify(t, provider, nil, []webhookMessage{textMessage("wamid.back", string(user), "ok")}, nil); err != nil {
		t.Fatal(err)
	}

	if got := receive(t, messages).ID(); got != "wamid.back" {
		t.Fatalf("received: got %q", got)
	}

	sent = graph.messages()
	if len(sent) != 3 {
		t.Fatalf("sent after reply: got %d messages, expected the template and both reminders", len(sent))
	}
	if sent[1].Text == nil || !strings.HasPrefix(sent[1].Text.Body, "Rappel") || sent[2].Text.Body != "Second rappel" {
		t.Errorf("held back messages: got %+v and %+v", sent[1].Text, sent[2].Text)
	}

	// The window is open again: the next message goes straight through.
	if _, err := provider.Send(ctx, courier.NewMessage(courier.RandomMessageID(), channel, nil, courier.WithMessageMainPart("direct"))); err != nil {
		t.Fatal(err)
	}
	if sent = graph.messages(); len(sent) != 4 || sent[3].Text == nil || sent[3].Text.Body != "direct" {
		t.Errorf("after reopening: got %+v", sent)
	}
}

func TestSendWithoutTemplateReportsClosedWindow(t *testing.T) {
	graph := newFakeGraph(t)

	windows := NewMemoryWindowStore()
	_ = windows.RecordInbound(context.Background(), "33600000001", time.Now().Add(-48*time.Hour))

	provider := newTestProvider(t, graph, WithWindowStore(windows))

	_, err := provider.Send(context.Background(), courier.NewMessage(courier.RandomMessageID(),
		courier.NewChannelRef("33600000001"), nil, courier.WithMessageMainPart("hello")))

	if !errors.Is(err, ErrWindowClosed) {
		t.Fatalf("Send: got %v, expected ErrWindowClosed", err)
	}
	if len(graph.messages()) != 0 {
		t.Error("nothing should have been sent")
	}
}

func TestSendFallsBackWhenWhatsAppRefuses(t *testing.T) {
	graph := newFakeGraph(t)
	graph.refuse = func(payload outgoing) *graphErrorBody {
		if payload.Type == "template" {
			return nil
		}
		var refusal graphErrorBody
		refusal.Error.Code = codeReengagement
		refusal.Error.Message = "Re-engagement message"
		return &refusal
	}

	var calls int
	provider := newTestProvider(t, graph, WithReengage(reengage(&calls)))

	id, err := provider.Send(context.Background(), courier.NewMessage(courier.RandomMessageID(),
		courier.NewChannelRef("33600000001"), nil, courier.WithMessageMainPart("hello")))
	if err != nil {
		t.Fatalf("Send: %+v", err)
	}

	if sent := graph.messages(); len(sent) != 1 || sent[0].Type != "template" || string(id) != "wamid.1" {
		t.Fatalf("sent: got %+v (id %q), expected the template", sent, id)
	}
}

func TestStatusFailureHoldsBackTheMessage(t *testing.T) {
	graph := newFakeGraph(t)

	var calls int
	provider := newTestProvider(t, graph, WithReengage(reengage(&calls)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := provider.Listen(ctx); err != nil {
		t.Fatal(err)
	}

	id, err := provider.Send(ctx, courier.NewMessage(courier.RandomMessageID(),
		courier.NewChannelRef("33600000001"), nil, courier.WithMessageMainPart("bulletin météo")))
	if err != nil {
		t.Fatal(err)
	}

	// WhatsApp accepted the message, then reports it refused.
	failure := webhookStatus{ID: string(id), Status: "failed", RecipientID: "33600000001"}
	failure.Errors = append(failure.Errors, struct {
		Code    int    `json:"code"`
		Title   string `json:"title"`
		Message string `json:"message"`
	}{Code: codeReengagement, Title: "Re-engagement message"})

	if err := notify(t, provider, nil, nil, []webhookStatus{failure}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sent := graph.messages(); len(sent) == 2 {
			if sent[1].Type != "template" || sent[1].Template.Components[0].Parameters[0].Text != "bulletin météo" {
				t.Fatalf("second message: got %+v, expected the template", sent[1])
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("no template sent after the failure: %+v", graph.messages())
}

func TestGroupMessages(t *testing.T) {
	graph := newFakeGraph(t)
	provider := newTestProvider(t, graph)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	messages, err := provider.Listen(ctx)
	if err != nil {
		t.Fatal(err)
	}

	raw := textMessage("wamid.grp", "33600000001", "@33600000099 quel temps demain ?")
	raw.FromUserID = "FR.abc123"
	raw.GroupID = "grp42"

	if err := notify(t, provider, []webhookContact{{WaID: "33600000001", UserID: "FR.abc123"}}, []webhookMessage{raw}, nil); err != nil {
		t.Fatal(err)
	}

	received := receive(t, messages)

	channel := received.Channel()
	if channel.ChannelID() != "group:grp42" || channel.Kind() != courier.ChannelKindGroup || channel.Name() != "Famille" {
		t.Errorf("channel: got %q %q %q", channel.ChannelID(), channel.Kind(), channel.Name())
	}
	if received.From().ID() != "FR.abc123" {
		t.Errorf("sender: got %q, expected the business-scoped user ID", received.From().ID())
	}

	self, err := provider.Self(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !courier.IsMentioned(received, self.ID()) {
		t.Errorf("expected the business number %q to be mentioned", self.ID())
	}

	if _, err := provider.Send(ctx, courier.NewMessage(courier.RandomMessageID(), channel, self, courier.WithMessageMainPart("Soleil"))); err != nil {
		t.Fatal(err)
	}

	sent := graph.messages()
	if len(sent) != 1 || sent[0].RecipientType != "group" || sent[0].To != "grp42" {
		t.Errorf("group send: got %+v", sent)
	}
}

func TestSendAddressesBusinessScopedUsers(t *testing.T) {
	graph := newFakeGraph(t)
	provider := newTestProvider(t, graph)

	if _, err := provider.Send(context.Background(), courier.NewMessage(courier.RandomMessageID(),
		courier.NewChannelRef("FR.abc123"), nil, courier.WithMessageMainPart("hello"))); err != nil {
		t.Fatal(err)
	}

	sent := graph.messages()
	if len(sent) != 1 || sent[0].Recipient != "FR.abc123" || sent[0].To != "" {
		t.Errorf("sent: got %+v, expected the BSUID in recipient", sent)
	}
}

func TestVoiceNotes(t *testing.T) {
	graph := newFakeGraph(t)
	provider := newTestProvider(t, graph)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	messages, err := provider.Listen(ctx)
	if err != nil {
		t.Fatal(err)
	}

	id := graph.store("audio/ogg; codecs=opus", "", []byte("opus"))

	raw := webhookMessage{ID: "wamid.voice", From: "33600000001", Type: "audio", Timestamp: strconv.FormatInt(time.Now().Unix(), 10)}
	raw.Audio = &webhookMedia{ID: id, MimeType: "audio/ogg; codecs=opus", Voice: true}

	if err := notify(t, provider, nil, []webhookMessage{raw}, nil); err != nil {
		t.Fatal(err)
	}

	received := receive(t, messages)

	attachments := courier.Attachments(received)
	if len(attachments) != 1 || !courier.IsVoiceNote(attachments[0]) {
		t.Fatalf("attachments: got %+v, expected one voice note", attachments)
	}

	content, err := courier.ReadPart(ctx, attachments[0])
	if err != nil || string(content) != "opus" {
		t.Fatalf("content: got %q, %v", content, err)
	}

	// An outgoing voice note keeps its text apart: an audio has no caption.
	reply := courier.NewMessage(courier.RandomMessageID(), received.Channel(), nil,
		courier.WithMessageMainPart("Voici la réponse"),
		courier.WithMessagePart(courier.NewAttachment("reply.ogg", "audio/ogg", courier.OpenerFromString("opus"),
			courier.WithAttachmentVoiceNote(time.Second))),
	)

	if _, err := provider.Send(ctx, reply); err != nil {
		t.Fatal(err)
	}

	sent := graph.messages()
	if len(sent) != 2 || sent[0].Text == nil || sent[1].Audio == nil || !sent[1].Audio.Voice || sent[1].Audio.Caption != "" {
		t.Errorf("sent: got %+v", sent)
	}
}

func TestSendRefusesUnsupportedMedia(t *testing.T) {
	graph := newFakeGraph(t)
	provider := newTestProvider(t, graph)

	_, err := provider.Send(context.Background(), courier.NewMessage(courier.RandomMessageID(),
		courier.NewChannelRef("33600000001"), nil,
		courier.WithMessagePart(courier.NewAttachment("archive.zip", "application/zip", courier.OpenerFromString("PK")))))

	if !errors.Is(err, ErrUnsupportedMedia) {
		t.Fatalf("Send: got %v, expected ErrUnsupportedMedia", err)
	}
}

func TestSplitText(t *testing.T) {
	text := strings.Repeat("a", 3000) + "\n\n" + strings.Repeat("b", 3000)

	chunks := splitText(text, maxTextLength)
	if len(chunks) != 2 || chunks[0] != strings.Repeat("a", 3000) || chunks[1] != strings.Repeat("b", 3000) {
		t.Errorf("chunks: got %d, expected a cut at the paragraph", len(chunks))
	}

	if got := splitText(strings.Repeat("é", 9000), maxTextLength); len(got) != 3 {
		t.Errorf("unbreakable text: got %d chunks, expected 3", len(got))
	}
}

func TestTypingIndicator(t *testing.T) {
	graph := newFakeGraph(t)
	provider := newTestProvider(t, graph)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	messages, err := provider.Listen(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if err := notify(t, provider, nil, []webhookMessage{textMessage("wamid.typing", "33600000001", "hello")}, nil); err != nil {
		t.Fatal(err)
	}
	received := receive(t, messages)

	if err := provider.SetStatus(ctx, courier.StatusTyping, received.Channel().ChannelID()); err != nil {
		t.Fatal(err)
	}

	graph.mu.Lock()
	defer graph.mu.Unlock()

	last := graph.sent[len(graph.sent)-1]
	if last.Status != "read" || last.MessageID != "wamid.typing" || last.TypingIndicator == nil {
		t.Errorf("typing: got %+v", last)
	}
}
