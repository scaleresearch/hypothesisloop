// Package apiclient is a thin HTTP client for the controlplane API — the same endpoints an agent
// uses, called here on behalf of a human typing commands instead of a loop.
package apiclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to the controlplane HTTP API at BaseURL, as whoever Token names.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{BaseURL: baseURL, Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// HTTPError is returned when the platform answers with a non-2xx status; it carries the response
// body so a caller can show the platform's own explanation rather than just the status code.
type HTTPError struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s %s -> HTTP %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// Do issues one request. body, when non-nil, is marshaled as the JSON request body. The response
// body is decoded into out (which may be nil to discard it).
func (c *Client) Do(method, path string, body any, out any) error {
	_, err := c.DoHeader(method, path, body, out)
	return err
}

// DoHeader is Do, also returning the response headers: a list's X-Total-Count.
func (c *Client) DoHeader(method, path string, body any, out any) (http.Header, error) {
	if c.BaseURL == "" {
		return nil, errors.New("no API URL given (set API_URL)")
	}
	if c.Token == "" {
		return nil, errors.New("no token given (set HL_TOKEN)")
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding request body: %w", err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, strings.TrimRight(c.BaseURL, "/")+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: reading response: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{Method: method, Path: path, Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	if out == nil || len(data) == 0 {
		return resp.Header, nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return nil, fmt.Errorf("%s %s: decoding response: %w", method, path, err)
	}
	return resp.Header, nil
}

// bytesTimeout is for a body of tens of megabytes over a slow link.
const bytesTimeout = 10 * time.Minute

// DoBytes issues one request whose body, when non-nil, and whose answer are bytes as they are:
// a git bundle going up or coming down.
func (c *Client) DoBytes(method, path string, body []byte) ([]byte, error) {
	if c.BaseURL == "" {
		return nil, errors.New("no API URL given (set API_URL)")
	}
	if c.Token == "" {
		return nil, errors.New("no token given (set HL_TOKEN)")
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, strings.TrimRight(c.BaseURL, "/")+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	client := *c.HTTP
	client.Timeout = bytesTimeout
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: reading response: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{Method: method, Path: path, Status: resp.StatusCode, Body: strings.TrimSpace(string(data[:min(len(data), 4<<10)]))}
	}
	return data, nil
}
