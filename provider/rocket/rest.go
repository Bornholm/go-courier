package rocket

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"sync"

	"github.com/bornholm/go-courier"
	"github.com/pkg/errors"
)

// credentials are the DDP login result, needed to authenticate REST calls.
// File upload and download go through the REST API: DDP carries the message
// metadata but not the file bytes.
type credentials struct {
	userID    string
	authToken string
}

// maxResponseBody bounds what is read back from the REST API: enough for an
// upload result or an error payload, never a whole file.
const maxResponseBody = 8 << 10

type restClient struct {
	baseURL *url.URL
	client  *http.Client

	mutex       sync.RWMutex
	credentials credentials
}

func newRESTClient(baseURL *url.URL, client *http.Client) *restClient {
	return &restClient{
		baseURL: baseURL,
		client:  client,
	}
}

func (c *restClient) setCredentials(userID, authToken string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	c.credentials = credentials{userID: userID, authToken: authToken}
}

func (c *restClient) getCredentials() (credentials, error) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	if c.credentials.authToken == "" {
		return credentials{}, errors.New("not authenticated against the rocket.chat REST API")
	}

	return c.credentials, nil
}

func (c *restClient) authenticate(req *http.Request) error {
	creds, err := c.getCredentials()
	if err != nil {
		return errors.WithStack(err)
	}

	req.Header.Set("X-Auth-Token", creds.authToken)
	req.Header.Set("X-User-Id", creds.userID)

	return nil
}

// download fetches a file previously uploaded to Rocket.Chat. path is the
// server relative link carried by the message, such as
// "/file-upload/<id>/<name>".
func (c *restClient) download(ctx context.Context, path string) (io.ReadCloser, error) {
	target, err := c.baseURL.Parse(path)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if err := c.authenticate(req); err != nil {
		return nil, errors.WithStack(err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, errors.Errorf("could not download %q: unexpected status %d", path, resp.StatusCode)
	}

	return resp.Body, nil
}

// upload posts a file to a room through the REST API, since DDP cannot carry
// file contents.
//
// Rocket.Chat takes it in two steps since the removal of rooms.upload: the
// bytes are stored first, then a second call turns the stored file into a
// message. Sending only the first half uploads a file nobody ever sees.
func (c *restClient) upload(ctx context.Context, roomID courier.ChannelID, attachment courier.Attachment, description string) error {
	fileID, err := c.uploadMedia(ctx, roomID, attachment)
	if err != nil {
		return errors.WithStack(err)
	}

	return errors.WithStack(c.confirmMedia(ctx, roomID, fileID, description))
}

// uploadMedia stores the file and returns the identifier Rocket.Chat gave it.
func (c *restClient) uploadMedia(ctx context.Context, roomID courier.ChannelID, attachment courier.Attachment) (string, error) {
	content, err := courier.ReadPart(ctx, attachment)
	if err != nil {
		return "", errors.WithStack(err)
	}

	var body bytes.Buffer

	writer := multipart.NewWriter(&body)

	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="`+courier.FilenameFor(attachment)+`"`)
	header.Set("Content-Type", attachment.ContentType())

	part, err := writer.CreatePart(header)
	if err != nil {
		return "", errors.WithStack(err)
	}

	if _, err := part.Write(content); err != nil {
		return "", errors.WithStack(err)
	}

	if err := writer.Close(); err != nil {
		return "", errors.WithStack(err)
	}

	req, err := c.newRequest(ctx, "/api/v1/rooms.media/"+url.PathEscape(string(roomID)), &body)
	if err != nil {
		return "", errors.WithStack(err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())

	var result struct {
		File struct {
			ID string `json:"_id"`
		} `json:"file"`
	}

	if err := c.do(req, courier.FilenameFor(attachment), &result); err != nil {
		return "", errors.WithStack(err)
	}

	if result.File.ID == "" {
		return "", errors.Errorf("could not upload %q: rocket.chat returned no file identifier",
			courier.FilenameFor(attachment))
	}

	return result.File.ID, nil
}

// confirmMedia turns an uploaded file into a message in the room. The text
// rides along as the message itself rather than as the file description: a
// description is shown under the file name, where a reply does not belong.
func (c *restClient) confirmMedia(ctx context.Context, roomID courier.ChannelID, fileID, description string) error {
	payload, err := json.Marshal(struct {
		Msg string `json:"msg,omitempty"`
	}{Msg: description})
	if err != nil {
		return errors.WithStack(err)
	}

	req, err := c.newRequest(ctx,
		"/api/v1/rooms.mediaConfirm/"+url.PathEscape(string(roomID))+"/"+url.PathEscape(fileID),
		bytes.NewReader(payload))
	if err != nil {
		return errors.WithStack(err)
	}

	req.Header.Set("Content-Type", "application/json")

	return errors.WithStack(c.do(req, fileID, nil))
}

// newRequest builds an authenticated POST against the REST API.
func (c *restClient) newRequest(ctx context.Context, path string, body io.Reader) (*http.Request, error) {
	target, err := c.baseURL.Parse(path)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), body)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if err := c.authenticate(req); err != nil {
		return nil, errors.WithStack(err)
	}

	return req, nil
}

// do runs the request and decodes result when one is expected. A refused call
// carries the reason Rocket.Chat gave: a bare status code says neither which
// endpoint disappeared nor which permission is missing.
func (c *restClient) do(req *http.Request, subject string, result any) error {
	resp, err := c.client.Do(req)
	if err != nil {
		return errors.WithStack(err)
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return errors.WithStack(err)
	}

	if resp.StatusCode != http.StatusOK {
		return errors.Errorf("could not upload %q: unexpected status %d on %s: %s",
			subject, resp.StatusCode, req.URL.Path, bytes.TrimSpace(body))
	}

	if result == nil {
		return nil
	}

	if err := json.Unmarshal(body, result); err != nil {
		return errors.Wrapf(err, "could not upload %q: unreadable response", subject)
	}

	return nil
}
