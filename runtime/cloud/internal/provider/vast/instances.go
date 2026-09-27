package vast

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

const listPageSize = 25

type instance struct {
	ID           int64   `json:"id"`
	Label        *string `json:"label"`
	ActualStatus *string `json:"actual_status"`
	StatusMsg    *string `json:"status_msg"`
	MachineID    int64   `json:"machine_id"`
	StartDate    float64 `json:"start_date"`
	GPUName      *string `json:"gpu_name"`
	CPUName      *string `json:"cpu_name"`
	PricePerHour float64 `json:"dph_total"`
}

// host is the machine an instance is on, empty until the vendor names one.
func (i instance) host() string {
	if i.MachineID == 0 {
		return ""
	}
	return strconv.FormatInt(i.MachineID, 10)
}

func text(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

const (
	// slowStart is how long an instance may be starting before List logs what its host says.
	// Image pulls on a cold host took up to six and a half minutes.
	slowStart = 8 * time.Minute
	// maxStatusMsg bounds the host's message in a log line.
	maxStatusMsg = 300
)

func (p *Provider) List(ctx context.Context, namePrefix string) ([]provider.Instance, error) {
	listed := []provider.Instance{}
	onAccount := map[string]bool{}
	token := ""
	for {
		query := url.Values{"limit": {strconv.Itoa(listPageSize)}}
		if token != "" {
			query.Set("after_token", token)
		}
		var page struct {
			Instances []instance `json:"instances"`
			NextToken *string    `json:"next_token"`
		}
		if _, err := p.call(ctx, http.MethodGet, "/api/v1/instances/?"+query.Encode(), nil, &page); err != nil {
			return nil, err
		}
		for _, found := range page.Instances {
			onAccount[strconv.FormatInt(found.ID, 10)] = true
			if found.Label == nil {
				continue
			}
			name := strings.TrimSuffix(*found.Label, stopMark)
			if !strings.HasPrefix(name, namePrefix) {
				continue
			}
			state, known := stateOf(found.ActualStatus)
			if !known {
				log.Printf("vast: instance %d (%s) has status %q, which this adapter does not know; reporting it as starting", found.ID, name, *found.ActualStatus)
			}
			startedAt := time.Unix(0, int64(found.StartDate*float64(time.Second)))
			if state == provider.StateStarting && time.Since(startedAt) > slowStart {
				logSlowStart(found, name, startedAt)
			}
			listed = append(listed, provider.Instance{
				ID:        strconv.FormatInt(found.ID, 10),
				Name:      name,
				State:     state,
				StartedAt: startedAt,
				Host:      found.host(), AcceleratorModel: text(found.GPUName), CPUModel: text(found.CPUName),
				PricePerHour: found.PricePerHour,
			})
		}
		if page.NextToken == nil || *page.NextToken == "" {
			p.held.keep(onAccount)
			return listed, nil
		}
		token = *page.NextToken
	}
}

// logSlowStart puts the host's own account of an instance that is not coming up where an
// operator reads it: provider.Instance has no place for it, and it is the only word on why (an
// image that cannot be pulled, a host that cannot hand over its GPU). It is logged on every List
// until the caller's boot timeout destroys the instance.
func logSlowStart(found instance, name string, startedAt time.Time) {
	message := "no status message"
	if found.StatusMsg != nil && strings.TrimSpace(*found.StatusMsg) != "" {
		message = strings.Join(strings.Fields(*found.StatusMsg), " ")
		if len(message) > maxStatusMsg {
			message = message[:maxStatusMsg]
		}
	}
	log.Printf("vast: instance %d (%s) on machine %d has been starting for %s: %s", found.ID, name, found.MachineID, time.Since(startedAt).Round(time.Second), message)
}

// stateOf maps the status the host last reported. It is null until the host has answered, and
// "offline" while the host is unreachable; in both the command is not known to be running, which
// is what starting means to a caller waiting on it. A status this adapter has never seen is
// starting as well, and not known: the instance stays listed, so it keeps its slot and can be
// destroyed, and the caller's boot timeout bounds it.
//
// "exited" may be passed on as it is, although the contract allows StateExited only once the
// whole output is readable. An instance is exited after its supervisor stopped it, which the
// supervisor does only after the command has ended and written its last line, or after the
// container ended some other way, when nothing is writing any more. And Logs never reads an old
// upload: every call has the host upload the output again and waits for that upload.
func stateOf(actualStatus *string) (state provider.State, known bool) {
	if actualStatus == nil {
		return provider.StateStarting, true
	}
	switch *actualStatus {
	case "loading", "created", "scheduling", "offline":
		return provider.StateStarting, true
	case "running":
		return provider.StateRunning, true
	case "exited":
		return provider.StateExited, true
	}
	return provider.StateStarting, false
}

// maxDestroyWait is the longest wait between two tries of a destroy the API refused as one call
// too many.
const maxDestroyWait = 15 * time.Second

func isGone(err error) bool {
	var failure *statusError
	return errors.As(err, &failure) && failure.Status == http.StatusNotFound && failure.Code == "no_such_instance"
}

// Destroy asks again when the API answers that the account called too often, which it does for
// a burst of a few calls: an instance left rented for a pass is paid for, and after the last
// pass of a process nobody asks again. It waits longer each time and gives up when ctx ends,
// with the vendor's refusal as the error.
func (p *Provider) Destroy(ctx context.Context, id string) error {
	var destroyed struct {
		Success bool   `json:"success"`
		Message string `json:"msg"`
	}
	var err error
	for wait := p.poll; ; wait = min(2*wait, maxDestroyWait) {
		_, err = p.call(ctx, http.MethodDelete, "/api/v0/instances/"+url.PathEscape(id)+"/", struct{}{}, &destroyed)
		var failure *statusError
		if !errors.As(err, &failure) || failure.Status != http.StatusTooManyRequests {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("vast: instance %s is still rented, the API refused every destroy as one call too many until time ran out: %w", id, err)
		case <-time.After(wait):
		}
	}
	if isGone(err) {
		p.held.release(id)
		return nil
	}
	if err != nil {
		return err
	}
	if !destroyed.Success {
		return fmt.Errorf("vast: destroying instance %s was refused: %s", id, destroyed.Message)
	}
	p.held.release(id)
	return nil
}
