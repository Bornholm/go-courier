package courier

import "errors"

var (
	ErrClosed   = errors.New("closed")
	ErrNotFound = errors.New("not found")
)
