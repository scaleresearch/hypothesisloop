// Package apiclient is a thin HTTP client for the controlplane API — the same endpoints an agent
// uses, called here on behalf of a human typing commands instead of a loop.
package apiclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to the controlplane HTTP API at BaseURL.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New builds a Client, defaulting BaseURL from the given value (falling back to
// http://localhost:8081 when empty, mirroring API_URL's default in the rest of this repo).
func New(baseURL string) *Client {
	if baseURL == "" {
		baseURL = "http://localhost:8081"
	}
	return &Client{BaseURL: baseURL, HTTP: &http.Client{Timeout: 30 * time.Second}}
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
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request body: %w", err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, strings.TrimRight(c.BaseURL, "/")+path, reader)
	if err != nil {
		return err
	}
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s %s: reading response: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{Method: method, Path: path, Status: resp.StatusCode, Body: string(data)}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s %s: decoding response: %w", method, path, err)
	}
	return nil
}
