package vendorapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const maxErrorBody = 512

// ErrTooLarge is in the error of an answer that was longer than its caller allows.
var ErrTooLarge = errors.New("answer is larger than anything asked for")

// StatusError is a non-2xx answer. Body is cut to maxErrorBody bytes.
type StatusError struct {
	Vendor string
	Method string
	Path   string
	Status int
	Header http.Header
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s: %s %s: HTTP %d: %s", e.Vendor, e.Method, e.Path, e.Status, e.Body)
}

// Do sends req and returns the body of a 2xx answer. The body is read up to maxBody bytes and a
// longer one is an error: part of what a vendor answers with is written by what runs inside an
// instance, and nothing an adapter asks for is legitimately larger than it says here.
func Do(client *http.Client, req *http.Request, vendor string, maxBody int64) ([]byte, http.Header, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %s %s: %w", vendor, req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, nil, &StatusError{Vendor: vendor, Method: req.Method, Path: req.URL.Path, Status: resp.StatusCode, Header: resp.Header, Body: string(raw)}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, nil, fmt.Errorf("%s: read %s %s: %w", vendor, req.Method, req.URL.Path, err)
	}
	if int64(len(raw)) > maxBody {
		return nil, nil, fmt.Errorf("%s: %s %s answered with more than %d bytes: %w", vendor, req.Method, req.URL.Path, maxBody, ErrTooLarge)
	}
	return raw, resp.Header, nil
}

// Truncate cuts text an error quotes down to a length that fits a log line.
func Truncate(text string) string {
	if len(text) > maxErrorBody {
		return text[:maxErrorBody]
	}
	return text
}

// GiveBack destroys an instance a Create made and could not finish, so that a failed Create
// leaves nothing behind, and returns cause with what became of the instance. ctx may be what
// ended the Create, so the destroy runs on its own time.
func GiveBack(ctx context.Context, destroy func(context.Context) error, timeout time.Duration, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	if err := destroy(ctx); err != nil {
		return fmt.Errorf("%w; destroying it failed and it may still be rented: %v", cause, err)
	}
	return fmt.Errorf("%w; it was destroyed", cause)
}
