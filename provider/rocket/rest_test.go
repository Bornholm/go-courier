package rocket

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/bornholm/go-courier"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *restClient {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %+v", server.URL, err)
	}

	client := newRESTClient(baseURL, server.Client())
	client.setCredentials("user-1", "token-1")

	return client
}

func testAttachment() courier.Attachment {
	return courier.NewAttachment("picture.png", "image/png", courier.OpenerFromString("PNG"))
}

// The upload is only finished once the stored file has been turned into a
// message: stopping after rooms.media leaves a file nobody sees.
func TestUploadConfirmsTheStoredFile(t *testing.T) {
	var paths []string
	var confirmed struct {
		Msg string `json:"msg"`
	}

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)

		if r.Header.Get("X-Auth-Token") != "token-1" || r.Header.Get("X-User-Id") != "user-1" {
			t.Errorf("call to %s is not authenticated", r.URL.Path)
		}

		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/rooms.media/"):
			io.WriteString(w, `{"file":{"_id":"file-9"},"success":true}`)
		case r.URL.Path == "/api/v1/rooms.mediaConfirm/room-1/file-9":
			if err := json.NewDecoder(r.Body).Decode(&confirmed); err != nil {
				t.Errorf("decoding the confirmation body: %+v", err)
			}
			io.WriteString(w, `{"success":true}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	if err := client.upload(context.Background(), "room-1", testAttachment(), "voici l'image"); err != nil {
		t.Fatalf("upload: %+v", err)
	}

	if len(paths) != 2 {
		t.Fatalf("calls: got %v, expected rooms.media then rooms.mediaConfirm", paths)
	}

	if paths[0] != "/api/v1/rooms.media/room-1" {
		t.Errorf("first call: got %q", paths[0])
	}

	if confirmed.Msg != "voici l'image" {
		t.Errorf("message sent with the file: got %q", confirmed.Msg)
	}
}

// The reason Rocket.Chat gave must survive: a bare status code says neither
// which endpoint disappeared nor which permission is missing.
func TestUploadReportsTheServerReason(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"success":false,"error":"unknown endpoint"}`)
	})

	err := client.upload(context.Background(), "room-1", testAttachment(), "")
	if err == nil {
		t.Fatal("upload: expected an error")
	}

	for _, expected := range []string{"picture.png", "404", "rooms.media", "unknown endpoint"} {
		if !strings.Contains(err.Error(), expected) {
			t.Errorf("error %q does not mention %q", err.Error(), expected)
		}
	}
}

func TestUploadRefusesAResponseWithoutFileID(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"success":true}`)
	})

	if err := client.upload(context.Background(), "room-1", testAttachment(), ""); err == nil {
		t.Fatal("upload: expected an error when no file identifier comes back")
	}
}
