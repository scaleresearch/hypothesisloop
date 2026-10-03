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

// unreachable says the platform gave no answer to a request of method: the request did not reach
// it, or a proxy in front of it answered 502, 503 or 504 in its place. It is the one place an
// agent reads what to do in an outage, so it says that too. A read changed nothing; a write may
// have been carried out before the answer was lost, and the line does not claim otherwise.
func unreachable(method string) string {
	const next = "A person runs it again later; an agent ends its turn, and an update arrives when the platform answers again"
	if method == http.MethodGet {
		return "the platform could not be reached, and this command changed nothing. " + next
	}
	return "the platform could not be reached, and whether this command took effect is not known: read what it changes before running it again. " + next
}

// notThePlatforms is the Body of an answer that is no JSON, which the platform's API never sends,
// said in one line in place of what may be a whole HTML page. The platform's router answers a
// method and path it does not serve in plain text: that is an hl older or newer than the
// platform, and no outage.
func notThePlatforms(method, path string, status int) string {
	switch status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return unreachable(method)
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return fmt.Sprintf("the platform serves no %s %s: this hl and the platform are of different versions", method, strings.SplitN(path, "?", 2)[0])
	}
	return "the answer is no JSON, so it is none of the platform's API: something in front of the platform answered in its place"
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
		return nil, fmt.Errorf("%s %s: %s (%w)", method, path, unreachable(method), err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: reading response: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body := strings.TrimSpace(string(data))
		if !json.Valid(data) {
			body = notThePlatforms(method, path, resp.StatusCode)
		}
		return nil, &HTTPError{Method: method, Path: path, Status: resp.StatusCode, Body: body}
	}
	if out == nil || len(data) == 0 {
		return resp.Header, nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return nil, fmt.Errorf("%s %s: decoding response: %w", method, path, err)
	}
	return resp.Header, nil
}
