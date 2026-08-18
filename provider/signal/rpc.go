package signal

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/pkg/errors"
)

// rpcClient is a minimal JSON-RPC 2.0 client over a stream connection
// (newline-delimited frames), matching what `signal-cli daemon --tcp` and
// `--socket` speak. Server-initiated notifications (method "receive") are
// pushed to the notifications channel; responses are matched to calls by id.
type rpcClient struct {
	conn net.Conn

	mu      sync.Mutex // guards writes to conn
	nextID  atomic.Int64
	pending sync.Map // id (string) -> chan rpcResponse

	notifications chan json.RawMessage

	closeOnce sync.Once
	closed    chan struct{}
	readErr   error
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

// dialRPC connects to the signal-cli daemon. Address forms:
// "tcp://host:port", "unix:///path/to/socket", or a bare "host:port" which is
// treated as TCP.
func dialRPC(ctx context.Context, address string) (*rpcClient, error) {
	network, target := "tcp", address
	switch {
	case strings.HasPrefix(address, "tcp://"):
		target = strings.TrimPrefix(address, "tcp://")
	case strings.HasPrefix(address, "unix://"):
		network, target = "unix", strings.TrimPrefix(address, "unix://")
	}

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, network, target)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	client := &rpcClient{
		conn: conn,
		// Buffered: receive notifications keep flowing while the consumer
		// converts the previous one; the daemon is never blocked long.
		notifications: make(chan json.RawMessage, 64),
		closed:        make(chan struct{}),
	}

	go client.readLoop()

	return client, nil
}

// readLoop reads frames until the connection dies, dispatching responses to
// their callers and notifications to the channel.
func (c *rpcClient) readLoop() {
	defer func() {
		close(c.notifications)
		// Unblock every in-flight call: the connection is gone, no response
		// will ever come.
		c.pending.Range(func(key, value any) bool {
			close(value.(chan rpcResponse))
			c.pending.Delete(key)
			return true
		})
	}()

	scanner := bufio.NewScanner(c.conn)
	// Attachments travel base64-encoded inside a single frame: the default
	// 64KiB limit would break on the first photo.
	scanner.Buffer(make([]byte, 0, 64*1024), 128*1024*1024)

	for scanner.Scan() {
		line := make([]byte, len(scanner.Bytes()))
		copy(line, scanner.Bytes())

		var resp rpcResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			continue // ligne non JSON : ignorée, le flux reste exploitable
		}

		if resp.ID != "" {
			if ch, ok := c.pending.LoadAndDelete(resp.ID); ok {
				ch.(chan rpcResponse) <- resp
			}
			continue
		}

		if resp.Method == "receive" {
			select {
			case c.notifications <- resp.Params:
			case <-c.closed:
				return
			}
		}
	}

	c.readErr = scanner.Err()
}

// call performs a JSON-RPC call and waits for its response.
func (c *rpcClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := fmt.Sprintf("%d", c.nextID.Add(1))

	payload, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, errors.WithStack(err)
	}
	payload = append(payload, '\n')

	ch := make(chan rpcResponse, 1)
	c.pending.Store(id, ch)
	defer c.pending.Delete(id)

	c.mu.Lock()
	_, err = c.conn.Write(payload)
	c.mu.Unlock()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	select {
	case <-ctx.Done():
		return nil, errors.WithStack(ctx.Err())
	case resp, ok := <-ch:
		if !ok {
			return nil, errors.New("connection to signal-cli closed")
		}
		if resp.Error != nil {
			return nil, errors.WithStack(resp.Error)
		}
		return resp.Result, nil
	}
}

func (c *rpcClient) close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.closed)
		err = c.conn.Close()
	})
	return err
}
