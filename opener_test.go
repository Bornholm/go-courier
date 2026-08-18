package courier

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/pkg/errors"
)

func readAll(t *testing.T, open PartOpener) string {
	t.Helper()

	reader, err := open(context.Background())
	if err != nil {
		t.Fatalf("open(ctx): %+v", err)
	}

	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("io.ReadAll(reader): %+v", err)
	}

	return string(data)
}

func TestOpenerFromBytesIsReplayable(t *testing.T) {
	open := OpenerFromBytes([]byte("hello"))

	if got := readAll(t, open); got != "hello" {
		t.Errorf("first read: got %q, expected %q", got, "hello")
	}

	if got := readAll(t, open); got != "hello" {
		t.Errorf("second read: got %q, expected %q", got, "hello")
	}
}

func TestOpenerFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "content.txt")
	if err := os.WriteFile(path, []byte("from disk"), 0o600); err != nil {
		t.Fatalf("os.WriteFile: %+v", err)
	}

	open := OpenerFromFile(path)

	if got := readAll(t, open); got != "from disk" {
		t.Errorf("first read: got %q, expected %q", got, "from disk")
	}

	if got := readAll(t, open); got != "from disk" {
		t.Errorf("second read: got %q, expected %q", got, "from disk")
	}
}

func TestOpenerOnce(t *testing.T) {
	open := OpenerOnce(io.NopCloser(bytes.NewBufferString("once")))

	if got := readAll(t, open); got != "once" {
		t.Errorf("first read: got %q, expected %q", got, "once")
	}

	if _, err := open(context.Background()); !errors.Is(err, ErrNotReplayable) {
		t.Errorf("second read: got error %v, expected ErrNotReplayable", err)
	}
}

func TestBufferedOpenerInMemory(t *testing.T) {
	var opened int

	source := func(ctx context.Context) (io.ReadCloser, error) {
		opened++
		return io.NopCloser(bytes.NewBufferString("buffered")), nil
	}

	open, closeFunc := BufferedOpener(source, 1024)
	defer closeFunc()

	if got := readAll(t, open); got != "buffered" {
		t.Errorf("first read: got %q, expected %q", got, "buffered")
	}

	if got := readAll(t, open); got != "buffered" {
		t.Errorf("second read: got %q, expected %q", got, "buffered")
	}

	// The underlying source is only consumed once, whatever the number of
	// reads.
	if opened != 1 {
		t.Errorf("source opened %d times, expected 1", opened)
	}
}

func TestBufferedOpenerSpillsToDisk(t *testing.T) {
	// Larger than the threshold, forcing a spill to a temporary file.
	content := bytes.Repeat([]byte("x"), 4096)

	source := func(ctx context.Context) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(content)), nil
	}

	open, closeFunc := BufferedOpener(source, 64)

	if got := readAll(t, open); got != string(content) {
		t.Errorf("first read: got %d bytes, expected %d", len(got), len(content))
	}

	if got := readAll(t, open); got != string(content) {
		t.Errorf("second read: got %d bytes, expected %d", len(got), len(content))
	}

	if err := closeFunc(); err != nil {
		t.Fatalf("closeFunc(): %+v", err)
	}

	// Closing twice must not fail, temporary file already removed.
	if err := closeFunc(); err != nil {
		t.Fatalf("second closeFunc(): %+v", err)
	}
}

func TestBufferedOpenerPropagatesSourceError(t *testing.T) {
	expected := errors.New("boom")

	source := func(ctx context.Context) (io.ReadCloser, error) {
		return nil, expected
	}

	open, closeFunc := BufferedOpener(source, 1024)
	defer closeFunc()

	if _, err := open(context.Background()); !errors.Is(err, expected) {
		t.Errorf("open(ctx): got error %v, expected %v", err, expected)
	}
}
