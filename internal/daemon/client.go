package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/sys/unix"
)

const requestTimeout = 30 * time.Second

// APIError is an error response from the daemon.
type APIError struct {
	Message string
	Status  int
}

func (e *APIError) Error() string { return e.Message }

// Client talks to a daemon over its unix socket.
type Client struct {
	http *http.Client
}

// NewClient returns a Client for the daemon listening on socket.
func NewClient(socket string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer

			return d.DialContext(ctx, "unix", socket)
		},
	}

	return &Client{http: &http.Client{Transport: transport}}
}

// List returns the running tunnels.
func (c *Client) List(ctx context.Context) ([]TunnelInfo, error) {
	var infos []TunnelInfo

	err := c.call(ctx, http.MethodGet, "/tunnels", nil, &infos)

	return infos, err
}

// Start runs the requested tunnels and returns them once all are online.
func (c *Client) Start(ctx context.Context, req *StartRequest) ([]TunnelInfo, error) {
	var infos []TunnelInfo

	err := c.call(ctx, http.MethodPost, "/tunnels", req, &infos)

	return infos, err
}

// Stop stops the tunnel called name.
// Apply makes the daemon's tunnels from req.Source match req; see ApplyRequest.
func (c *Client) Apply(ctx context.Context, req *ApplyRequest) (*ApplyResult, error) {
	var res ApplyResult
	if err := c.call(ctx, http.MethodPost, "/apply", req, &res); err != nil {
		return nil, err
	}

	return &res, nil
}

func (c *Client) Stop(ctx context.Context, name string) error {
	return c.call(ctx, http.MethodDelete, "/tunnels/"+url.PathEscape(name), nil, nil)
}

// StopAll stops every tunnel and returns their names.
func (c *Client) StopAll(ctx context.Context) ([]string, error) {
	var names []string

	err := c.call(ctx, http.MethodDelete, "/tunnels", nil, &names)

	return names, err
}

// Shutdown asks the daemon to stop all tunnels and exit.
func (c *Client) Shutdown(ctx context.Context) error {
	return c.call(ctx, http.MethodPost, "/shutdown", nil, nil)
}

// Logs calls fn with each stored log line of the tunnel called name.
func (c *Client) Logs(ctx context.Context, name string, fn func(LogLine)) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	return c.streamLogs(ctx, "/tunnels/"+url.PathEscape(name)+"/logs", fn)
}

// FollowLogs calls fn with stored and then new log lines until ctx is canceled or the tunnel stops.
func (c *Client) FollowLogs(ctx context.Context, name string, fn func(LogLine)) error {
	return c.streamLogs(ctx, "/tunnels/"+url.PathEscape(name)+"/logs?follow=true", fn)
}

func (c *Client) streamLogs(ctx context.Context, path string, fn func(LogLine)) error {
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer closeBody(resp)

	dec := jsontext.NewDecoder(resp.Body)

	for {
		var line LogLine

		err := json.UnmarshalDecode(dec, &line)
		if errors.Is(err, io.EOF) || ctx.Err() != nil {
			return nil
		}

		if err != nil {
			return fmt.Errorf("read logs: %w", err)
		}

		fn(line)
	}
}

func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	resp, err := c.do(ctx, method, path, in)
	if err != nil {
		return err
	}
	defer closeBody(resp)

	if out == nil {
		return nil
	}

	if err := json.UnmarshalRead(resp.Body, out); err != nil {
		return fmt.Errorf("decode daemon response: %w", err)
	}

	return nil
}

// do sends a request and turns connection failures into ErrNotRunning and error statuses into APIError.
func (c *Client) do(ctx context.Context, method, path string, in any) (*http.Response, error) {
	var body io.Reader

	if in != nil {
		pr, pw := io.Pipe()

		go func() { pw.CloseWithError(json.MarshalWrite(pw, in)) }()

		body = pr
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://parrot"+path, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	resp, err := c.http.Do(req)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, unix.ECONNREFUSED) {
		return nil, ErrNotRunning
	}

	if err != nil {
		return nil, fmt.Errorf("daemon request: %w", err)
	}

	if resp.StatusCode < http.StatusBadRequest {
		return resp, nil
	}

	defer closeBody(resp)

	var apiErr apiError
	if err := json.UnmarshalRead(resp.Body, &apiErr); err != nil || apiErr.Error == "" {
		apiErr.Error = resp.Status
	}

	return nil, &APIError{Message: apiErr.Error, Status: resp.StatusCode}
}

func closeBody(resp *http.Response) {
	// Bodies here are fully read or abandoned on purpose; a close error has no consequence.
	_ = resp.Body.Close()
}
