package podexec

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/moby/moby/client"
)

// FetchLogTail returns the last maxLines lines of experimentID's own container's current
// stdout/stderr, read live from the container engine (never cached, never stored by this
// executor itself — see agentloop.LogTailer). Returns (nil, nil) rather than an error if no
// managed container exists for experimentID yet: an ordinary transient state for a job that was
// just admitted, not a failure worth logging every reconcile tick.
func (e *Executor) FetchLogTail(ctx context.Context, experimentID string, maxLines int) ([]string, error) {
	containers, err := e.listManagedContainers(ctx)
	if err != nil {
		return nil, fmt.Errorf("podexec.FetchLogTail: list containers: %w", err)
	}
	var containerID string
	for _, c := range containers {
		if c.Labels[LabelExperimentID] == experimentID {
			containerID = c.ID
			break
		}
	}
	if containerID == "" {
		return nil, nil
	}

	tail := fmt.Sprintf("%d", maxLines)
	stream, err := e.docker.ContainerLogs(ctx, containerID, client.ContainerLogsOptions{
		ShowStdout: true, ShowStderr: true, Tail: tail,
	})
	if err != nil {
		return nil, nil
	}
	defer stream.Close()

	lines, err := demuxLogLines(stream)
	if err != nil {
		return nil, fmt.Errorf("podexec.FetchLogTail: read log stream for %s: %w", experimentID, err)
	}
	return lines, nil
}

// demuxLogLines splits the container engine's multiplexed stdout/stderr log stream (no TTY
// attached to these job containers, so every frame is prefixed by an 8-byte header: 1 byte
// stream type, 3 bytes padding, 4-byte big-endian payload length — see
// github.com/moby/moby/api/pkg/stdcopy, not vendored here since this is the only place that
// needs it) into plain text lines, stdout and stderr interleaved in arrival order.
func demuxLogLines(r io.Reader) ([]string, error) {
	var lines []string
	header := make([]byte, 8)
	br := bufio.NewReader(r)
	for {
		if _, err := io.ReadFull(br, header); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return lines, err
		}
		size := binary.BigEndian.Uint32(header[4:8])
		payload := make([]byte, size)
		if _, err := io.ReadFull(br, payload); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return lines, err
		}
		scanner := bufio.NewScanner(bytes.NewReader(payload))
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
	}
	return lines, nil
}
