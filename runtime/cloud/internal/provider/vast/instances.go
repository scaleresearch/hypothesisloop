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
	StartDate    float64 `json:"start_date"`
}

func (p *Provider) List(ctx context.Context, namePrefix string) ([]provider.Instance, error) {
	listed := []provider.Instance{}
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
			listed = append(listed, provider.Instance{
				ID:        strconv.FormatInt(found.ID, 10),
				Name:      name,
				State:     state,
				StartedAt: time.Unix(0, int64(found.StartDate*float64(time.Second))),
			})
		}
		if page.NextToken == nil || *page.NextToken == "" {
			return listed, nil
		}
		token = *page.NextToken
	}
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

func isGone(err error) bool {
	var failure *statusError
	return errors.As(err, &failure) && failure.Status == http.StatusNotFound && failure.Code == "no_such_instance"
}

func (p *Provider) Destroy(ctx context.Context, id string) error {
	var destroyed struct {
		Success bool   `json:"success"`
		Message string `json:"msg"`
	}
	_, err := p.call(ctx, http.MethodDelete, "/api/v0/instances/"+url.PathEscape(id)+"/", struct{}{}, &destroyed)
	if isGone(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !destroyed.Success {
		return fmt.Errorf("vast: destroying instance %s was refused: %s", id, destroyed.Message)
	}
	return nil
}
