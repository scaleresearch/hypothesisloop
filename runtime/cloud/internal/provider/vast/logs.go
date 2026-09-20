package vast

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// logDeliveryTimeout is how long one Logs call waits for the host's upload. Uploads arrived 1.3
// to 2.5 seconds after the request on every healthy instance measured, and a caller reads its
// instances one after another, so one host that does not deliver must not hold up the rest.
const logDeliveryTimeout = 10 * time.Second

var errLogNotDelivered = errors.New("not delivered yet")

// Logs asks the instance's host to upload its container output and reads the upload. The API
// answers at once with the address the upload will appear at; the address is the same on every
// call, so an upload is only taken once it is newer than the request that asked for it.
func (p *Provider) Logs(ctx context.Context, id string, tail int) ([]string, error) {
	if tail <= 0 {
		return nil, fmt.Errorf("vast: logs of instance %s: tail must be positive, got %d", id, tail)
	}
	var requested struct {
		Success   bool   `json:"success"`
		ResultURL string `json:"result_url"`
		Message   string `json:"msg"`
	}
	path := "/api/v0/instances/request_logs/" + url.PathEscape(id) + "/"
	header, err := p.call(ctx, http.MethodPut, path, map[string]string{"tail": strconv.Itoa(tail)}, &requested)
	if err != nil {
		return nil, err
	}
	if !requested.Success || requested.ResultURL == "" {
		return nil, fmt.Errorf("vast: logs of instance %s were refused: %s", id, requested.Message)
	}
	requestedAt, err := http.ParseTime(header.Get("Date"))
	if err != nil {
		return nil, fmt.Errorf("vast: logs of instance %s: API answer carries no usable Date: %w", id, err)
	}
	body, err := p.awaitUpload(ctx, id, requested.ResultURL, requestedAt)
	if err != nil {
		return nil, err
	}
	return logLines(id, body), nil
}

func (p *Provider) awaitUpload(ctx context.Context, id, uploadURL string, requestedAt time.Time) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.logWait)
	defer cancel()
	ticker := time.NewTicker(p.poll)
	defer ticker.Stop()
	for {
		body, err := p.fetchUpload(ctx, uploadURL, requestedAt)
		if !errors.Is(err, errLogNotDelivered) {
			return body, err
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("vast: the host of instance %s did not deliver its logs within %s: %w", id, p.logWait, ctx.Err())
		case <-ticker.C:
		}
	}
}

// fetchUpload reads the upload once. Until the host has delivered, the store answers 403 for an
// address it holds nothing at, or still serves what an earlier request left there.
func (p *Provider) fetchUpload(ctx context.Context, uploadURL string, requestedAt time.Time) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uploadURL, nil)
	if err != nil {
		return "", fmt.Errorf("vast: build log download: %w", err)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", errLogNotDelivered
		}
		return "", fmt.Errorf("vast: log download: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden, http.StatusNotFound:
		return "", errLogNotDelivered
	default:
		return "", fmt.Errorf("vast: log download: HTTP %d", resp.StatusCode)
	}
	uploadedAt, err := http.ParseTime(resp.Header.Get("Last-Modified"))
	if err != nil {
		return "", fmt.Errorf("vast: log download carries no usable Last-Modified: %w", err)
	}
	if uploadedAt.Before(requestedAt) {
		return "", errLogNotDelivered
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("vast: read log download: %w", err)
	}
	return string(raw), nil
}

// logLines splits an upload into lines. A host with no container for the instance yet (the image
// is still being pulled, or could not be) uploads the engine's complaint in place of output; the
// command has written nothing then.
func logLines(id, body string) []string {
	body = strings.TrimRight(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	if body == "" || body == "Error response from daemon: No such container: C."+id {
		return []string{}
	}
	return strings.Split(body, "\n")
}
