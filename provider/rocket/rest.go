package rocket

import (
	"bytes"
	"context"
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
func (c *restClient) upload(ctx context.Context, roomID courier.ChannelID, attachment courier.Attachment, description string) error {
	content, err := courier.ReadPart(ctx, attachment)
	if err != nil {
		return errors.WithStack(err)
	}

	var body bytes.Buffer

	writer := multipart.NewWriter(&body)

	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="`+courier.FilenameFor(attachment)+`"`)
	header.Set("Content-Type", attachment.ContentType())

	part, err := writer.CreatePart(header)
	if err != nil {
		return errors.WithStack(err)
	}

	if _, err := part.Write(content); err != nil {
		return errors.WithStack(err)
	}

	if description != "" {
		if err := writer.WriteField("msg", description); err != nil {
			return errors.WithStack(err)
		}
	}

	if err := writer.Close(); err != nil {
		return errors.WithStack(err)
	}

	target, err := c.baseURL.Parse("/api/v1/rooms.upload/" + url.PathEscape(string(roomID)))
	if err != nil {
		return errors.WithStack(err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), &body)
	if err != nil {
		return errors.WithStack(err)
	}

	if err := c.authenticate(req); err != nil {
		return errors.WithStack(err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.client.Do(req)
	if err != nil {
		return errors.WithStack(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return errors.Errorf("could not upload %q: unexpected status %d",
			courier.FilenameFor(attachment), resp.StatusCode)
	}

	return nil
}
