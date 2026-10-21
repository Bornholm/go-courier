package whatsappcloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/pkg/errors"
)

// Error codes of the Graph API the provider reacts to.
const (
	// codeReengagement is reported when more than 24 hours have passed
	// since the user last wrote.
	codeReengagement = 131047
)

// APIError is an error reported by the Graph API.
type APIError struct {
	Status    int
	Code      int
	Subcode   int
	Type      string
	Message   string
	Details   string
	FBTraceID string
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("graph api: http %d, code %d", e.Status, e.Code)
	if e.Subcode != 0 {
		msg += fmt.Sprintf(", subcode %d", e.Subcode)
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.Details != "" {
		msg += " (" + e.Details + ")"
	}
	return msg
}

// IsWindowClosed reports whether err is WhatsApp refusing a free-form
// message because the customer service window is closed.
func IsWindowClosed(err error) bool {
	if errors.Is(err, ErrWindowClosed) {
		return true
	}

	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Code == codeReengagement
}

type graphErrorBody struct {
	Error struct {
		Message      string `json:"message"`
		Type         string `json:"type"`
		Code         int    `json:"code"`
		ErrorSubcode int    `json:"error_subcode"`
		FBTraceID    string `json:"fbtrace_id"`
		ErrorData    struct {
			Details string `json:"details"`
		} `json:"error_data"`
	} `json:"error"`
}

func (p *Provider) url(path string) string {
	return strings.TrimRight(p.opts.BaseURL, "/") + "/" + p.opts.APIVersion + "/" + strings.TrimLeft(path, "/")
}

// do performs an authenticated request and decodes its JSON answer into out,
// when not nil.
func (p *Provider) do(req *http.Request, out any) error {
	req.Header.Set("Authorization", "Bearer "+p.opts.AccessToken)

	res, err := p.opts.HTTPClient.Do(req)
	if err != nil {
		return errors.WithStack(err)
	}
	defer res.Body.Close()

	// Graph API answers are small; the bound only guards against a broken
	// proxy streaming garbage.
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return errors.WithStack(err)
	}

	if res.StatusCode >= 300 {
		apiErr := &APIError{Status: res.StatusCode}

		var decoded graphErrorBody
		if json.Unmarshal(body, &decoded) == nil {
			apiErr.Code = decoded.Error.Code
			apiErr.Subcode = decoded.Error.ErrorSubcode
			apiErr.Type = decoded.Error.Type
			apiErr.Message = decoded.Error.Message
			apiErr.Details = decoded.Error.ErrorData.Details
			apiErr.FBTraceID = decoded.Error.FBTraceID
		}

		return errors.WithStack(apiErr)
	}

	if out == nil {
		return nil
	}

	if err := json.Unmarshal(body, out); err != nil {
		return errors.Wrap(err, "could not decode graph api answer")
	}

	return nil
}

func (p *Provider) callJSON(ctx context.Context, method, path string, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return errors.WithStack(err)
		}
		body = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, p.url(path), body)
	if err != nil {
		return errors.WithStack(err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return p.do(req, out)
}

// outgoing is the body of POST /<PHONE_NUMBER_ID>/messages.
type outgoing struct {
	MessagingProduct string `json:"messaging_product"`
	RecipientType    string `json:"recipient_type,omitempty"`
	// To holds a phone number or a group identifier.
	To string `json:"to,omitempty"`
	// Recipient holds a business-scoped user ID.
	Recipient string `json:"recipient,omitempty"`
	Type      string `json:"type,omitempty"`

	Context  *outgoingContext `json:"context,omitempty"`
	Text     *outgoingText    `json:"text,omitempty"`
	Image    *outgoingMedia   `json:"image,omitempty"`
	Audio    *outgoingMedia   `json:"audio,omitempty"`
	Video    *outgoingMedia   `json:"video,omitempty"`
	Document *outgoingMedia   `json:"document,omitempty"`
	Template *outgoingTmpl    `json:"template,omitempty"`

	// Status and the fields below mark a message as read and show the
	// typing indicator.
	Status          string           `json:"status,omitempty"`
	MessageID       string           `json:"message_id,omitempty"`
	TypingIndicator *typingIndicator `json:"typing_indicator,omitempty"`
}

type outgoingContext struct {
	MessageID string `json:"message_id"`
}

type outgoingText struct {
	Body       string `json:"body"`
	PreviewURL bool   `json:"preview_url,omitempty"`
}

type outgoingMedia struct {
	ID       string `json:"id"`
	Caption  string `json:"caption,omitempty"`
	Filename string `json:"filename,omitempty"`
	Voice    bool   `json:"voice,omitempty"`
}

type outgoingTmpl struct {
	Name       string               `json:"name"`
	Language   outgoingTmplLanguage `json:"language"`
	Components []outgoingTmplComp   `json:"components,omitempty"`
}

type outgoingTmplLanguage struct {
	Code string `json:"code"`
}

type outgoingTmplComp struct {
	Type       string              `json:"type"`
	Parameters []outgoingTmplParam `json:"parameters"`
}

type outgoingTmplParam struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type typingIndicator struct {
	Type string `json:"type"`
}

type sendResult struct {
	Messages []struct {
		ID string `json:"id"`
	} `json:"messages"`
}

func (p *Provider) postMessage(ctx context.Context, payload outgoing) (string, error) {
	payload.MessagingProduct = "whatsapp"

	var result sendResult
	if err := p.callJSON(ctx, http.MethodPost, p.opts.PhoneNumberID+"/messages", payload, &result); err != nil {
		return "", errors.WithStack(err)
	}

	if len(result.Messages) == 0 || result.Messages[0].ID == "" {
		return "", errors.New("graph api accepted the message without returning its identifier")
	}

	return result.Messages[0].ID, nil
}

// uploadMedia uploads content and returns the media identifier to reference
// in a message.
func (p *Provider) uploadMedia(ctx context.Context, filename, contentType string, content io.Reader) (string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	if err := writer.WriteField("messaging_product", "whatsapp"); err != nil {
		return "", errors.WithStack(err)
	}
	if err := writer.WriteField("type", contentType); err != nil {
		return "", errors.WithStack(err)
	}

	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	header.Set("Content-Type", contentType)

	part, err := writer.CreatePart(header)
	if err != nil {
		return "", errors.WithStack(err)
	}
	if _, err := io.Copy(part, content); err != nil {
		return "", errors.WithStack(err)
	}
	if err := writer.Close(); err != nil {
		return "", errors.WithStack(err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url(p.opts.PhoneNumberID+"/media"), &body)
	if err != nil {
		return "", errors.WithStack(err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	var result struct {
		ID string `json:"id"`
	}
	if err := p.do(req, &result); err != nil {
		return "", errors.WithStack(err)
	}
	if result.ID == "" {
		return "", errors.New("graph api accepted the upload without returning its identifier")
	}

	return result.ID, nil
}

// downloadMedia fetches the content of an incoming media. WhatsApp hands a
// media identifier; resolving it yields a short-lived URL, itself only
// reachable with the access token.
func (p *Provider) downloadMedia(ctx context.Context, mediaID string) (io.ReadCloser, error) {
	var media struct {
		URL      string `json:"url"`
		MimeType string `json:"mime_type"`
		FileSize int64  `json:"file_size"`
	}
	if err := p.callJSON(ctx, http.MethodGet, mediaID, nil, &media); err != nil {
		return nil, errors.WithStack(err)
	}
	if media.URL == "" {
		return nil, errors.New("graph api returned no url for the media")
	}
	if media.FileSize > p.opts.MaxMediaSize {
		return nil, errors.Errorf("media of %d bytes exceeds the %d bytes limit", media.FileSize, p.opts.MaxMediaSize)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, media.URL, nil)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	req.Header.Set("Authorization", "Bearer "+p.opts.AccessToken)

	res, err := p.opts.HTTPClient.Do(req)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	if res.StatusCode >= 300 {
		res.Body.Close()
		return nil, errors.Errorf("media download: http %d", res.StatusCode)
	}

	return &limitedReadCloser{
		Reader: io.LimitReader(res.Body, p.opts.MaxMediaSize+1),
		closer: res.Body,
		limit:  p.opts.MaxMediaSize,
	}, nil
}

// limitedReadCloser fails, rather than silently truncating, a body longer
// than the limit.
type limitedReadCloser struct {
	io.Reader
	closer io.Closer
	limit  int64
	read   int64
}

func (r *limitedReadCloser) Read(buf []byte) (int, error) {
	n, err := r.Reader.Read(buf)
	r.read += int64(n)
	if r.read > r.limit {
		return n, errors.Errorf("media exceeds the %d bytes limit", r.limit)
	}
	return n, err
}

func (r *limitedReadCloser) Close() error {
	return r.closer.Close()
}
