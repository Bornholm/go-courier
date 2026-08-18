package courier

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/pkg/errors"
)

// DefaultMaxInMemorySize is the threshold above which BufferedOpener spills
// to a temporary file instead of keeping the content in memory.
const DefaultMaxInMemorySize int64 = 4 << 20 // 4MiB

// ErrNotReplayable is returned when a part backed by a single use source is
// read more than once.
var ErrNotReplayable = errors.New("part content is not replayable")

// PartOpener lazily opens the content of a message part. Implementations
// should be replayable: calling the opener twice must yield the same content.
// Use BufferedOpener to make a single use source replayable.
type PartOpener func(ctx context.Context) (io.ReadCloser, error)

// OpenerFromBytes returns a replayable opener over an in-memory buffer.
func OpenerFromBytes(data []byte) PartOpener {
	return func(ctx context.Context) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}
}

// OpenerFromString returns a replayable opener over a string.
func OpenerFromString(data string) PartOpener {
	return func(ctx context.Context) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(data)), nil
	}
}

// OpenerFromFile returns a replayable opener over a file on disk. The file
// must outlive the message.
func OpenerFromFile(path string) PartOpener {
	return func(ctx context.Context) (io.ReadCloser, error) {
		file, err := os.Open(path)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		return file, nil
	}
}

// OpenerOnce wraps a single use reader. The second call returns
// ErrNotReplayable rather than an empty or corrupted stream. Prefer wrapping
// it in BufferedOpener when the content may be read more than once.
func OpenerOnce(reader io.ReadCloser) PartOpener {
	var (
		mutex    sync.Mutex
		consumed bool
	)

	return func(ctx context.Context) (io.ReadCloser, error) {
		mutex.Lock()
		defer mutex.Unlock()

		if consumed {
			return nil, errors.WithStack(ErrNotReplayable)
		}

		consumed = true

		return reader, nil
	}
}

// BufferedOpener materializes the content of open on first read and serves
// every subsequent read from that copy, turning a single use source into a
// replayable one. Contents larger than maxInMemory are spilled to a temporary
// file, removed when the returned CloseFunc is called.
//
// A maxInMemory of zero or less falls back to DefaultMaxInMemorySize.
func BufferedOpener(open PartOpener, maxInMemory int64) (PartOpener, CloseFunc) {
	if maxInMemory <= 0 {
		maxInMemory = DefaultMaxInMemorySize
	}

	var (
		mutex    sync.Mutex
		resolved bool
		buffer   []byte
		tempPath string
		openErr  error
	)

	materialize := func(ctx context.Context) error {
		if resolved {
			return openErr
		}

		resolved = true

		reader, err := open(ctx)
		if err != nil {
			openErr = errors.WithStack(err)
			return openErr
		}

		defer reader.Close()

		var head bytes.Buffer

		copied, err := io.Copy(&head, io.LimitReader(reader, maxInMemory))
		if err != nil {
			openErr = errors.WithStack(err)
			return openErr
		}

		if copied < maxInMemory {
			buffer = head.Bytes()
			return nil
		}

		// Content reached the threshold, spill everything to a temporary file.
		file, err := os.CreateTemp("", "courier-part-*")
		if err != nil {
			openErr = errors.WithStack(err)
			return openErr
		}

		defer file.Close()

		if _, err := io.Copy(file, io.MultiReader(&head, reader)); err != nil {
			os.Remove(file.Name())
			openErr = errors.WithStack(err)
			return openErr
		}

		tempPath = file.Name()

		return nil
	}

	opener := func(ctx context.Context) (io.ReadCloser, error) {
		mutex.Lock()
		defer mutex.Unlock()

		if err := materialize(ctx); err != nil {
			return nil, errors.WithStack(err)
		}

		if tempPath != "" {
			file, err := os.Open(tempPath)
			if err != nil {
				return nil, errors.WithStack(err)
			}

			return file, nil
		}

		return io.NopCloser(bytes.NewReader(buffer)), nil
	}

	closeFunc := func() error {
		mutex.Lock()
		defer mutex.Unlock()

		buffer = nil

		if tempPath == "" {
			return nil
		}

		path := tempPath
		tempPath = ""

		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return errors.WithStack(err)
		}

		return nil
	}

	return opener, closeFunc
}

// CloseFunc releases the resources held by a buffered opener.
type CloseFunc func() error
