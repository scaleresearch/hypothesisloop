package runpod

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
)

// The marks a pod's name carries after the instance name, in this order: stopMark once Stop asked
// the command to end, then upMark once the supervisor started the command or exitMark once the
// command ended. A name never holds a dot, so no mark can be part of one.
const (
	stopMark = ".stop"
	upMark   = ".up"
	exitMark = ".exited"
)

// marked is a pod's name read back: the instance name and its marks.
type marked struct {
	name    string
	stopped bool
	// up is upMark or exitMark, empty before the command started.
	up string
}

func parseName(podName string) marked {
	var m marked
	for _, mark := range []string{upMark, exitMark} {
		if trimmed, found := strings.CutSuffix(podName, mark); found {
			podName, m.up = trimmed, mark
			break
		}
	}
	m.name, m.stopped = strings.CutSuffix(podName, stopMark)
	return m
}

func (m marked) String() string {
	name := m.name
	if m.stopped {
		name += stopMark
	}
	return name + m.up
}

type pod struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Status       string  `json:"status"`
	DataCenterID *string `json:"dataCenterId"`
	Cost         float64 `json:"cost"`
	CreatedAt    string  `json:"createdAt"`
	StartedAt    *string `json:"startedAt"`
	GPU          *struct {
		ID    string `json:"id"`
		Count int    `json:"count"`
	} `json:"gpu"`
	CPU *struct {
		ID string `json:"id"`
	} `json:"cpu"`
	GlobalNetworking struct {
		Enabled bool   `json:"enabled"`
		IP      string `json:"ip"`
	} `json:"globalNetworking"`
}

const (
	// listPageSize is the largest page the API serves.
	listPageSize = 1000
	// statusTerminated is a pod the vendor has deleted.
	statusTerminated = "TERMINATED"
)

func (p *Provider) List(ctx context.Context, namePrefix string) ([]provider.Instance, error) {
	listed := []provider.Instance{}
	cursor := ""
	for {
		query := url.Values{"limit": {fmt.Sprint(listPageSize)}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var page struct {
			Pods       []pod `json:"pods"`
			Pagination struct {
				NextCursor  *string `json:"nextCursor"`
				HasNextPage bool    `json:"hasNextPage"`
			} `json:"pagination"`
		}
		if err := p.call(ctx, http.MethodGet, "/v2/pods?"+query.Encode(), nil, &page); err != nil {
			return nil, err
		}
		for _, found := range page.Pods {
			if found.Status == statusTerminated || !strings.HasPrefix(parseName(found.Name).name, namePrefix) {
				continue
			}
			i, err := p.instance(found)
			if err != nil {
				return nil, err
			}
			listed = append(listed, i)
		}
		if !page.Pagination.HasNextPage || page.Pagination.NextCursor == nil {
			return listed, nil
		}
		cursor = *page.Pagination.NextCursor
	}
}

// Get reads the pod by its id. The API answers one it no longer has with 404.
func (p *Provider) Get(ctx context.Context, id string) (provider.Instance, bool, error) {
	var found pod
	err := p.call(ctx, http.MethodGet, "/v2/pods/"+url.PathEscape(id), nil, &found)
	if status(err) == http.StatusNotFound {
		return provider.Instance{}, false, nil
	}
	if err != nil {
		return provider.Instance{}, false, err
	}
	if found.Status == statusTerminated {
		return provider.Instance{}, false, nil
	}
	i, err := p.instance(found)
	return i, err == nil, err
}

// instance is found as a caller sees it, under the name it was created with. The vendor names no
// machine a pod is on, so Host stays empty.
func (p *Provider) instance(found pod) (provider.Instance, error) {
	startedAt, err := started(found)
	if err != nil {
		return provider.Instance{}, err
	}
	name := parseName(found.Name)
	state, known := stateOf(found.Status, name)
	if !known {
		p.log("pod %s (%s) has status %q, which this adapter does not know; reporting it as unknown", found.ID, name.name, found.Status)
	}
	i := provider.Instance{
		ID: found.ID, Name: name.name, State: state,
		StartedAt:    startedAt,
		PricePerHour: found.Cost, Architecture: architecture,
		PrivateAddress: found.GlobalNetworking.IP,
	}
	if found.GPU != nil {
		i.AcceleratorModel = found.GPU.ID
	}
	if found.CPU != nil {
		i.CPUModel = found.CPU.ID
	}
	return i, nil
}

// started is when the pod started, or was created when it has not.
func started(found pod) (time.Time, error) {
	at := found.CreatedAt
	if found.StartedAt != nil {
		at = *found.StartedAt
	}
	parsed, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return time.Time{}, fmt.Errorf("runpod: pod %s: %w", found.ID, err)
	}
	return parsed, nil
}

// stateOf maps the vendor's status and the supervisor's marks. A RUNNING pod is starting until the
// supervisor has started the command and exited once it has ended. EXITED is a pod the vendor
// stopped, which removes the container and its output: the adapter never stops one, so that is
// unknown, as is ERROR and a status this adapter has never seen. The pod stays listed either way,
// so it keeps its slot and can be destroyed.
func stateOf(status string, name marked) (state provider.State, known bool) {
	switch status {
	case "PROVISIONING", "STARTING":
		return provider.StateStarting, true
	case "RUNNING":
		switch name.up {
		case upMark:
			return provider.StateRunning, true
		case exitMark:
			return provider.StateExited, true
		}
		return provider.StateStarting, true
	case "EXITED", "ERROR":
		return provider.StateUnknown, true
	}
	return provider.StateUnknown, false
}

// Destroy terminates the pod. One the API no longer has is gone already.
func (p *Provider) Destroy(ctx context.Context, id string) error {
	err := p.call(ctx, http.MethodDelete, "/v2/pods/"+url.PathEscape(id), nil, nil)
	if status(err) == http.StatusNotFound {
		return nil
	}
	return err
}
