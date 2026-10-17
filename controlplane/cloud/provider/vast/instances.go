package vast

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/cloud/provider"
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
			if found.Label == nil || !strings.HasPrefix(strings.TrimSuffix(*found.Label, stopMark), namePrefix) {
				continue
			}
			listed = append(listed, p.instance(found))
		}
		if page.NextToken == nil || *page.NextToken == "" {
			p.held.keep(onAccount)
			return listed, nil
		}
		token = *page.NextToken
	}
}

// instance is found as a caller sees it, under the name it was created with.
func (p *Provider) instance(found instance) provider.Instance {
	name := ""
	if found.Label != nil {
		name = strings.TrimSuffix(*found.Label, stopMark)
	}
	state, known := stateOf(found.ActualStatus)
	if !known {
		p.log("instance %d (%s) has status %q, which this adapter does not know; reporting it as unknown", found.ID, name, *found.ActualStatus)
	}
	startedAt := time.Unix(0, int64(found.StartDate*float64(time.Second)))
	if state == provider.StateStarting && time.Since(startedAt) > slowStart {
		p.logSlowStart(found, name, startedAt)
	}
	return provider.Instance{
		ID:        strconv.FormatInt(found.ID, 10),
		Name:      name,
		State:     state,
		StartedAt: startedAt,
		Host:      found.host(), AcceleratorModel: text(found.GPUName), CPUModel: text(found.CPUName),
		PricePerHour: found.PricePerHour,
	}
}

// Get reads the instance by its id. The API answers an id it does not have, or no longer has,
// with no instance and 200.
func (p *Provider) Get(ctx context.Context, id string) (provider.Instance, bool, error) {
	var shown struct {
		Instances *instance `json:"instances"`
	}
	_, err := p.call(ctx, http.MethodGet, "/api/v0/instances/"+url.PathEscape(id)+"/", nil, &shown)
	if isGone(err) {
		return provider.Instance{}, false, nil
	}
	if err != nil {
		return provider.Instance{}, false, err
	}
	if shown.Instances == nil {
		return provider.Instance{}, false, nil
	}
	return p.instance(*shown.Instances), true, nil
}

// logSlowStart puts the host's own account of an instance that is not coming up where an
// operator reads it: provider.Instance has no place for it, and it is the only word on why (an
// image that cannot be pulled, a host that cannot hand over its GPU). It is logged on every List
// until the caller's boot timeout destroys the instance.
func (p *Provider) logSlowStart(found instance, name string, startedAt time.Time) {
	message := "no status message"
	if found.StatusMsg != nil && strings.TrimSpace(*found.StatusMsg) != "" {
		message = strings.Join(strings.Fields(*found.StatusMsg), " ")
		if len(message) > maxStatusMsg {
			message = message[:maxStatusMsg]
		}
	}
	p.log("instance %d (%s) on machine %d has been starting for %s: %s", found.ID, name, found.MachineID, time.Since(startedAt).Round(time.Second), message)
}

// stateOf maps the status the host last reported. It is null until the host has answered, which
// is starting. It is "offline" while the host is unreachable, at any point of the instance's
// life, so the command may be hours in: that is unknown, as is a status this adapter has never
// seen, which is also not known. The instance stays listed either way, so it keeps its slot and
// can be destroyed.
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
	case "loading", "created", "scheduling":
		return provider.StateStarting, true
	case "running":
		return provider.StateRunning, true
	case "exited":
		return provider.StateExited, true
	case "offline":
		return provider.StateUnknown, true
	}
	return provider.StateUnknown, false
}

func isGone(err error) bool {
	var failure *statusError
	return errors.As(err, &failure) && failure.Status == http.StatusNotFound && failure.Code == "no_such_instance"
}

// Destroy is asked once. A refusal, one call too many among them, is the error, and the caller's
// next pass asks again.
func (p *Provider) Destroy(ctx context.Context, id string) error {
	var destroyed struct {
		Success bool   `json:"success"`
		Message string `json:"msg"`
	}
	_, err := p.call(ctx, http.MethodDelete, "/api/v0/instances/"+url.PathEscape(id)+"/", struct{}{}, &destroyed)
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
