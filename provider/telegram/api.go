package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"

	"github.com/pkg/errors"
)

// APIError is an error reported by the Bot API.
type APIError struct {
	// Code mirrors the HTTP status: 400 for a malformed request, 401 for a
	// revoked token, 403 when the bot was blocked or kicked, 409 when
	// another client polls the same bot or a webhook is set, 429 when
	// rate-limited.
	Code        int
	Description string
	// RetryAfter is the number of seconds to wait, on a 429.
	RetryAfter int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram bot api: error %d: %s", e.Code, e.Description)
}

type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

type apiUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

type apiChat struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

type apiEntity struct {
	Type   string   `json:"type"`
	Offset int      `json:"offset"`
	Length int      `json:"length"`
	User   *apiUser `json:"user"`
}

type apiPhotoSize struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int64  `json:"file_size"`
}

// apiFile covers document, audio, voice, video, video_note and animation:
// the fields each of them lacks simply stay empty.
type apiFile struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
	Duration int    `json:"duration"`
}

type apiMessage struct {
	MessageID       int            `json:"message_id"`
	MessageThreadID int            `json:"message_thread_id"`
	IsTopicMessage  bool           `json:"is_topic_message"`
	From            *apiUser       `json:"from"`
	SenderChat      *apiChat       `json:"sender_chat"`
	Date            int64          `json:"date"`
	Chat            apiChat        `json:"chat"`
	ReplyToMessage  *apiMessage    `json:"reply_to_message"`
	Text            string         `json:"text"`
	Entities        []apiEntity    `json:"entities"`
	Caption         string         `json:"caption"`
	CaptionEntities []apiEntity    `json:"caption_entities"`
	Photo           []apiPhotoSize `json:"photo"`
	Document        *apiFile       `json:"document"`
	Audio           *apiFile       `json:"audio"`
	Voice           *apiFile       `json:"voice"`
	Video           *apiFile       `json:"video"`
	VideoNote       *apiFile       `json:"video_note"`
	Animation       *apiFile       `json:"animation"`
}

type apiReactionType struct {
	Type  string `json:"type"`
	Emoji string `json:"emoji"`
}

type apiReactionUpdated struct {
	Chat        apiChat           `json:"chat"`
	MessageID   int               `json:"message_id"`
	User        *apiUser          `json:"user"`
	ActorChat   *apiChat          `json:"actor_chat"`
	Date        int64             `json:"date"`
	OldReaction []apiReactionType `json:"old_reaction"`
	NewReaction []apiReactionType `json:"new_reaction"`
}

type apiUpdate struct {
	UpdateID        int64               `json:"update_id"`
	Message         *apiMessage         `json:"message"`
	MessageReaction *apiReactionUpdated `json:"message_reaction"`
}

type replyParameters struct {
	MessageID int `json:"message_id"`
	// AllowSendingWithoutReply keeps the answer when the message it
	// replies to was deleted in the meantime.
	AllowSendingWithoutReply bool `json:"allow_sending_without_reply"`
}

func (p *Provider) methodURL(method string) string {
	return strings.TrimRight(p.opts.BaseURL, "/") + "/bot" + p.opts.Token + "/" + method
}

func (p *Provider) fileURL(path string) string {
	return strings.TrimRight(p.opts.BaseURL, "/") + "/file/bot" + p.opts.Token + "/" + strings.TrimLeft(path, "/")
}

// call performs a Bot API method with a JSON payload and decodes its result
// into out, when not nil.
func (p *Provider) call(ctx context.Context, method string, payload any, out any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return errors.WithStack(err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.methodURL(method), bytes.NewReader(data))
	if err != nil {
		return errors.WithStack(redact(err))
	}
	req.Header.Set("Content-Type", "application/json")

	return p.do(req, method, out)
}

// upload is a file to send with callMultipart.
type upload struct {
	field       string
	filename    string
	contentType string
	content     io.Reader
}

// callMultipart performs a Bot API method uploading a file.
func (p *Provider) callMultipart(ctx context.Context, method string, fields map[string]string, file upload, out any) error {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			return errors.WithStack(err)
		}
	}

	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, file.field, file.filename))
	header.Set("Content-Type", file.contentType)

	part, err := writer.CreatePart(header)
	if err != nil {
		return errors.WithStack(err)
	}
	if _, err := io.Copy(part, file.content); err != nil {
		return errors.WithStack(err)
	}
	if err := writer.Close(); err != nil {
		return errors.WithStack(err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.methodURL(method), &body)
	if err != nil {
		return errors.WithStack(redact(err))
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	return p.do(req, method, out)
}

func (p *Provider) do(req *http.Request, method string, out any) error {
	res, err := p.opts.HTTPClient.Do(req)
	if err != nil {
		return errors.Wrapf(redact(err), "telegram bot api: %s", method)
	}
	defer res.Body.Close()

	// Bot API answers are small; the bound only guards against a broken
	// proxy streaming garbage.
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return errors.Wrapf(redact(err), "telegram bot api: %s", method)
	}

	var decoded apiResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return errors.Errorf("telegram bot api: %s: http %d, undecodable answer", method, res.StatusCode)
	}

	if !decoded.OK {
		apiErr := &APIError{Code: decoded.ErrorCode, Description: decoded.Description}
		if apiErr.Code == 0 {
			apiErr.Code = res.StatusCode
		}
		if decoded.Parameters != nil {
			apiErr.RetryAfter = decoded.Parameters.RetryAfter
		}
		return errors.WithStack(apiErr)
	}

	if out == nil {
		return nil
	}

	if err := json.Unmarshal(decoded.Result, out); err != nil {
		return errors.Wrapf(err, "telegram bot api: %s: could not decode result", method)
	}

	return nil
}

// download fetches a file the bot received. Telegram hands a file
// identifier; getFile resolves it into a path, valid for an hour, under the
// token-bearing file URL. Bots may download files up to 20MB.
func (p *Provider) download(ctx context.Context, fileID string) (io.ReadCloser, error) {
	var file struct {
		FilePath string `json:"file_path"`
	}
	if err := p.call(ctx, "getFile", map[string]any{"file_id": fileID}, &file); err != nil {
		return nil, errors.WithStack(err)
	}
	if file.FilePath == "" {
		return nil, errors.New("telegram bot api: getFile returned no path, the file may exceed the 20MB bot limit")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.fileURL(file.FilePath), nil)
	if err != nil {
		return nil, errors.WithStack(redact(err))
	}

	res, err := p.opts.HTTPClient.Do(req)
	if err != nil {
		return nil, errors.Wrap(redact(err), "telegram file download")
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, errors.Errorf("telegram file download: http %d", res.StatusCode)
	}

	return res.Body, nil
}

// redact strips the request URL from a transport error. The bot token is
// part of every Bot API URL, and net/http quotes the URL in its errors:
// without this, a mere network outage would write the token in the logs.
func redact(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return errors.Errorf("%s: %v", urlErr.Op, urlErr.Err)
	}
	return err
}
