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
			state, err := stateOf(found.ActualStatus)
			if err != nil {
				return nil, fmt.Errorf("vast: instance %d (%s): %w", found.ID, name, err)
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
// is what starting means to a caller waiting on it.
func stateOf(actualStatus *string) (provider.State, error) {
	if actualStatus == nil {
		return provider.StateStarting, nil
	}
	switch *actualStatus {
	case "loading", "created", "scheduling", "offline":
		return provider.StateStarting, nil
	case "running":
		return provider.StateRunning, nil
	case "exited":
		return provider.StateExited, nil
	}
	return "", fmt.Errorf("unknown status %q", *actualStatus)
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
