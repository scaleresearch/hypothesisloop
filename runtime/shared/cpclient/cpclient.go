// Package cpclient is the one way runtime software calls the control plane: a base URL, the
// bearer token every request carries, JSON both ways, and a refusal in the control plane's own
// words.
package cpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// refusalBodyLimit bounds how much of a refusal's body is read.
const refusalBodyLimit = 512

// Client calls one control plane API under BaseURL, such as API_URL + "/internal/clusters/" +
// name.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	// Header is sent on every request beside the token, such as the version of the caller.
	Header http.Header
	// MaxAnswerBytes bounds an answer held in memory; 0 bounds nothing.
	MaxAnswerBytes int
}

// Refusal is a request the control plane answered with a status other than 2xx. Reason and
// Message are its {"reason", "error"} body, when it has one; Body is what it said, trimmed.
type Refusal struct {
	Status  int
	Reason  string
	Message string
	Body    string
}

func (r *Refusal) Error() string {
	return fmt.Sprintf("control plane answered %d: %s", r.Status, r.Body)
}

// Do sends body, JSON-encoded when not nil, with method to path under BaseURL, and decodes a 2xx
// answer into out when out is not nil. Any other status is a *Refusal.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, payload)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, values := range c.Header {
		req.Header[name] = values
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return refusal(resp)
	}
	if out == nil {
		return nil
	}
	reader := io.Reader(resp.Body)
	if c.MaxAnswerBytes > 0 {
		reader = io.LimitReader(resp.Body, int64(c.MaxAnswerBytes)+1)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	if c.MaxAnswerBytes > 0 && len(raw) > c.MaxAnswerBytes {
		return fmt.Errorf("answer is larger than %d bytes", c.MaxAnswerBytes)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode answer: %w", err)
	}
	return nil
}

func refusal(resp *http.Response) *Refusal {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, refusalBodyLimit))
	said := struct {
		Reason  string `json:"reason"`
		Message string `json:"error"`
	}{}
	_ = json.Unmarshal(body, &said)
	return &Refusal{Status: resp.StatusCode, Reason: said.Reason, Message: said.Message, Body: string(bytes.TrimSpace(body))}
}
