package whatsappcloud

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// The webhook payload, as documented in the "messages" field reference.
// Only what the provider uses is decoded.

type webhookPayload struct {
	Object string         `json:"object"`
	Entry  []webhookEntry `json:"entry"`
}

type webhookEntry struct {
	ID      string          `json:"id"`
	Changes []webhookChange `json:"changes"`
}

type webhookChange struct {
	Field string       `json:"field"`
	Value webhookValue `json:"value"`
}

type webhookValue struct {
	MessagingProduct string `json:"messaging_product"`
	Metadata         struct {
		DisplayPhoneNumber string `json:"display_phone_number"`
		PhoneNumberID      string `json:"phone_number_id"`
	} `json:"metadata"`
	Contacts []webhookContact `json:"contacts"`
	Messages []webhookMessage `json:"messages"`
	Statuses []webhookStatus  `json:"statuses"`
}

type webhookContact struct {
	Profile struct {
		Name string `json:"name"`
	} `json:"profile"`
	WaID   string `json:"wa_id"`
	UserID string `json:"user_id"`
}

type webhookMessage struct {
	From       string `json:"from"`
	FromUserID string `json:"from_user_id"`
	GroupID    string `json:"group_id"`
	ID         string `json:"id"`
	Timestamp  string `json:"timestamp"`
	Type       string `json:"type"`

	Context *struct {
		From string `json:"from"`
		ID   string `json:"id"`
	} `json:"context"`

	Text *struct {
		Body string `json:"body"`
	} `json:"text"`

	Image    *webhookMedia `json:"image"`
	Audio    *webhookMedia `json:"audio"`
	Video    *webhookMedia `json:"video"`
	Document *webhookMedia `json:"document"`
	Sticker  *webhookMedia `json:"sticker"`

	Reaction *struct {
		MessageID string `json:"message_id"`
		Emoji     string `json:"emoji"`
	} `json:"reaction"`

	Button *struct {
		Text    string `json:"text"`
		Payload string `json:"payload"`
	} `json:"button"`

	Interactive *struct {
		Type        string `json:"type"`
		ButtonReply *struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"button_reply"`
		ListReply *struct {
			ID          string `json:"id"`
			Title       string `json:"title"`
			Description string `json:"description"`
		} `json:"list_reply"`
	} `json:"interactive"`

	Location *struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
		Name      string  `json:"name"`
		Address   string  `json:"address"`
	} `json:"location"`
}

type webhookMedia struct {
	ID       string `json:"id"`
	MimeType string `json:"mime_type"`
	SHA256   string `json:"sha256"`
	Caption  string `json:"caption"`
	Filename string `json:"filename"`
	Voice    bool   `json:"voice"`
}

type webhookStatus struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	Timestamp       string `json:"timestamp"`
	RecipientID     string `json:"recipient_id"`
	RecipientUserID string `json:"recipient_user_id"`
	Errors          []struct {
		Code    int    `json:"code"`
		Title   string `json:"title"`
		Message string `json:"message"`
	} `json:"errors"`
}

// ServeHTTP implements http.Handler: it is the webhook endpoint to declare
// in the Meta application, mounted wherever the application sees fit.
//
//	GET   verification handshake (hub.mode, hub.verify_token, hub.challenge)
//	POST  notifications, signed with X-Hub-Signature-256
//
// Notifications are acknowledged as soon as they are queued. When the
// provider is not listening, or lags too far behind, the endpoint answers
// 503 and Meta delivers the notification again later.
func (p *Provider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		p.handleVerify(w, r)
	case http.MethodPost:
		p.handleNotification(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
	}
}

func (p *Provider) handleVerify(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	// Without a configured token, nothing can be verified: an empty one
	// would match any request omitting it.
	if p.opts.VerifyToken == "" || query.Get("hub.mode") != "subscribe" ||
		subtle.ConstantTimeCompare([]byte(query.Get("hub.verify_token")), []byte(p.opts.VerifyToken)) != 1 {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, query.Get("hub.challenge"))
}

func (p *Provider) handleNotification(w http.ResponseWriter, r *http.Request) {
	if p.opts.AppSecret == "" {
		slog.ErrorContext(r.Context(), "whatsapp cloud webhook refused: no app secret configured")
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, p.opts.MaxWebhookSize))
	if err != nil {
		http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
		return
	}

	if !validSignature(p.opts.AppSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		slog.WarnContext(r.Context(), "whatsapp cloud webhook refused: invalid signature")
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}

	var payload webhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	if payload.Object != "whatsapp_business_account" {
		// Not for us, but well signed: acknowledge so that Meta stops
		// retrying.
		w.WriteHeader(http.StatusOK)
		return
	}

	if !p.enqueue(payload) {
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// validSignature checks the "sha256=<hex>" HMAC of the raw body.
func validSignature(secret string, body []byte, header string) bool {
	signature, found := strings.CutPrefix(header, "sha256=")
	if !found {
		return false
	}

	expected, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)

	return hmac.Equal(mac.Sum(nil), expected)
}
