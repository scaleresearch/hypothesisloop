package e2b

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"

	"github.com/scaleresearch/hypothesisloop/controlplane/cloud/provider/vendorapi"
)

// envd is the vendor's process inside one sandbox, reached with that sandbox's own token. Every
// call runs as the template's default user. That is the image's USER when the image names one;
// for an image without one it is the vendor's own account "user", which has passwordless sudo.
type envd struct {
	p     *Provider
	base  string
	token string
}

const (
	// maxFrame bounds one message of a process stream. envd sends output in pieces of a few
	// tens of kilobytes.
	maxFrame = 1 << 20

	// maxUploadAnswer bounds what envd says about a file it took or a signal it sent.
	maxUploadAnswer = 64 << 10
)

func (e envd) request(ctx context.Context, method, path string, contentType string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, e.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("e2b: build %s %s: %w", method, path, err)
	}
	req.Header.Set("X-Access-Token", e.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req, nil
}

func filePath(path string) string {
	return "/files?" + url.Values{"path": {path}}.Encode()
}

// readFile returns the content of a file of at most limit bytes, and found false for a path
// that does not exist. A larger file is an error and is not read to its end.
func (e envd) readFile(ctx context.Context, path string, limit int64) (content string, found bool, err error) {
	req, err := e.request(ctx, http.MethodGet, filePath(path), "", nil)
	if err != nil {
		return "", false, err
	}
	raw, _, err := vendorapi.Do(e.p.http, req, vendor, limit)
	if hasStatus(err, http.StatusNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(raw), true, nil
}

// writeFile uploads a file. envd does not log what a file holds.
func (e envd) writeFile(ctx context.Context, path, content string) error {
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	part, err := writer.CreateFormFile("file", "file")
	if err != nil {
		return fmt.Errorf("e2b: encode upload of %s: %w", path, err)
	}
	if _, err := io.WriteString(part, content); err != nil {
		return fmt.Errorf("e2b: encode upload of %s: %w", path, err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("e2b: encode upload of %s: %w", path, err)
	}
	req, err := e.request(ctx, http.MethodPost, filePath(path), writer.FormDataContentType(), form.Bytes())
	if err != nil {
		return err
	}
	_, _, err = vendorapi.Do(e.p.http, req, vendor, maxUploadAnswer)
	return err
}

type processConfig struct {
	Cmd  string   `json:"cmd"`
	Args []string `json:"args"`
}

type startRequest struct {
	Process processConfig `json:"process"`
	Tag     string        `json:"tag,omitempty"`
	Stdin   bool          `json:"stdin"`
}

// processEvent is one message of the stream a process start answers with.
type processEvent struct {
	Event struct {
		Start *struct {
			PID int `json:"pid"`
		} `json:"start"`
		Data *struct {
			Stdout []byte `json:"stdout"`
			Stderr []byte `json:"stderr"`
		} `json:"data"`
		End *struct {
			Exited bool   `json:"exited"`
			Status string `json:"status"`
		} `json:"end"`
	} `json:"event"`
}

// The process service speaks the Connect protocol: a stream is a sequence of envelopes, each a
// flag byte, a four byte length and a JSON message, and the envelope flagged as the end carries
// the error, if there was one.
const (
	streamContentType = "application/connect+json"
	endOfStream       = 0x02
)

func envelope(message any) ([]byte, error) {
	encoded, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	framed := make([]byte, 5, 5+len(encoded))
	binary.BigEndian.PutUint32(framed[1:], uint32(len(encoded)))
	return append(framed, encoded...), nil
}

// start starts a process and hands each event of its stream to each, until each returns false or
// the stream ends. A process keeps running after its stream is dropped.
func (e envd) start(ctx context.Context, process startRequest, each func(processEvent) bool) error {
	const path = "/process.Process/Start"
	body, err := envelope(process)
	if err != nil {
		return fmt.Errorf("e2b: encode process start: %w", err)
	}
	req, err := e.request(ctx, http.MethodPost, path, streamContentType, body)
	if err != nil {
		return err
	}
	req.Header.Set("Connect-Protocol-Version", "1")
	resp, err := e.p.http.Do(req)
	if err != nil {
		return fmt.Errorf("e2b: POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxFrame))
		return &vendorapi.StatusError{Vendor: vendor, Method: http.MethodPost, Path: path, Status: resp.StatusCode, Body: vendorapi.Truncate(string(raw))}
	}
	for {
		var head [5]byte
		if _, err := io.ReadFull(resp.Body, head[:]); err != nil {
			return fmt.Errorf("e2b: the process stream broke off before its end: %w", err)
		}
		size := binary.BigEndian.Uint32(head[1:])
		if size > maxFrame {
			return fmt.Errorf("e2b: the process stream announces a message of %d bytes, and none is larger than %d", size, maxFrame)
		}
		message := make([]byte, size)
		if _, err := io.ReadFull(resp.Body, message); err != nil {
			return fmt.Errorf("e2b: the process stream broke off inside a message: %w", err)
		}
		if head[0]&endOfStream != 0 {
			var end struct {
				Error *struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(message, &end); err != nil {
				return fmt.Errorf("e2b: decode the end of the process stream: %w", err)
			}
			if end.Error != nil {
				return fmt.Errorf("e2b: process start failed: %s: %s", end.Error.Code, end.Error.Message)
			}
			return nil
		}
		var event processEvent
		if err := json.Unmarshal(message, &event); err != nil {
			return fmt.Errorf("e2b: decode a process event: %w", err)
		}
		if !each(event) {
			return nil
		}
	}
}

// run runs a short process to its end and returns what it wrote to stdout, at most maxOutput
// bytes of it. A process that wrote more, or that did not end with success, is an error.
func (e envd) run(ctx context.Context, maxOutput int, cmd string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	status, oversized := "", false
	err := e.start(ctx, startRequest{Process: processConfig{Cmd: cmd, Args: args}}, func(event processEvent) bool {
		if data := event.Event.Data; data != nil {
			stdout.Write(data.Stdout)
			if stderr.Len() < maxFrame {
				stderr.Write(data.Stderr)
			}
		}
		if end := event.Event.End; end != nil {
			status = end.Status
		}
		oversized = stdout.Len() > maxOutput
		return !oversized
	})
	switch {
	case err != nil:
		return "", err
	case oversized:
		return "", fmt.Errorf("e2b: the process wrote more than %d bytes", maxOutput)
	case status != "exit status 0":
		return "", fmt.Errorf("e2b: the process ended with %q: %s", status, vendorapi.Truncate(stderr.String()))
	}
	return stdout.String(), nil
}

// signal sends the process started under tag SIGTERM. No process running under the tag is not
// an error.
func (e envd) signal(ctx context.Context, tag string) error {
	body, err := json.Marshal(map[string]any{"process": map[string]string{"tag": tag}, "signal": "SIGNAL_SIGTERM"})
	if err != nil {
		return fmt.Errorf("e2b: encode signal: %w", err)
	}
	req, err := e.request(ctx, http.MethodPost, "/process.Process/SendSignal", "application/json", body)
	if err != nil {
		return err
	}
	_, _, err = vendorapi.Do(e.p.http, req, vendor, maxUploadAnswer)
	if hasStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}
