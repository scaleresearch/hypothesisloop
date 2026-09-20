package vast

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider/vendorapi"
)

const listedTimeout = 30 * time.Second

type createRequest struct {
	ClientID string `json:"client_id"`
	Image    string `json:"image"`
	// RunType "args" runs the image as is: no ssh or jupyter wrapper replaces its entrypoint.
	RunType string            `json:"runtype"`
	Label   string            `json:"label"`
	Disk    int64             `json:"disk"`
	Env     map[string]string `json:"env"`
	// OnStart is the entrypoint override under run type "args", one executable and no arguments.
	OnStart string   `json:"onstart"`
	Args    []string `json:"args"`
}

func (p *Provider) Create(ctx context.Context, spec provider.Spec) (string, error) {
	if err := validateSpec(spec); err != nil {
		return "", err
	}
	offers, err := p.cheapestOffers(ctx, spec)
	if err != nil {
		return "", err
	}
	id, err := p.rentFirstAvailable(ctx, offers, newCreateRequest(spec))
	if err != nil {
		return "", err
	}
	if err := p.awaitListed(ctx, id, spec.Name); err != nil {
		destroy := func(ctx context.Context) error { return p.Destroy(ctx, id) }
		return "", vendorapi.GiveBack(ctx, destroy, requestTimeout, err)
	}
	return id, nil
}

// rentFirstAvailable rents the cheapest offer that still exists. An offer the search returned
// after someone else took it is passed over, since nothing was rented and the next one fits the
// spec as well; any other refusal ends the Create.
func (p *Provider) rentFirstAvailable(ctx context.Context, offers []offer, req createRequest) (string, error) {
	var taken error
	for _, candidate := range offers {
		id, err := p.rent(ctx, candidate, req)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, errOfferTaken) {
			return "", err
		}
		taken = err
	}
	return "", fmt.Errorf("vast: every one of the %d cheapest offers for %q was taken: %w", len(offers), req.Label, taken)
}

var errOfferTaken = errors.New("offer is no longer available")

const offerTakenMark = "no_such_ask"

func (p *Provider) rent(ctx context.Context, chosen offer, req createRequest) (string, error) {
	var created struct {
		Success  bool   `json:"success"`
		Contract int64  `json:"new_contract"`
		Message  string `json:"msg"`
	}
	path := "/api/v0/asks/" + strconv.FormatInt(chosen.ID, 10) + "/"
	_, err := p.call(ctx, http.MethodPut, path, req, &created)
	var failure *statusError
	switch {
	case errors.As(err, &failure) && strings.Contains(failure.Message, offerTakenMark):
		return "", fmt.Errorf("%w: %w", errOfferTaken, err)
	case err != nil:
		return "", err
	case !created.Success && strings.Contains(created.Message, offerTakenMark):
		return "", fmt.Errorf("vast: renting offer %d: %w: %s", chosen.ID, errOfferTaken, created.Message)
	case !created.Success || created.Contract == 0:
		return "", fmt.Errorf("vast: renting offer %d for %q was refused: %s", chosen.ID, req.Label, created.Message)
	}
	return strconv.FormatInt(created.Contract, 10), nil
}

func (p *Provider) listed(ctx context.Context, id, name string) (bool, error) {
	instances, err := p.List(ctx, name)
	if err != nil {
		return false, err
	}
	for _, found := range instances {
		if found.ID == id && found.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// awaitListed holds Create until the rented instance is in List, which is what tells a caller
// with no memory of this call that the name is taken.
func (p *Provider) awaitListed(ctx context.Context, id, name string) error {
	ctx, cancel := context.WithTimeout(ctx, listedTimeout)
	defer cancel()
	ticker := time.NewTicker(p.poll)
	defer ticker.Stop()
	for {
		listed, err := p.listed(ctx, id, name)
		if err != nil {
			return fmt.Errorf("vast: instance %s was rented for %q and could not be confirmed in the list: %w", id, name, err)
		}
		if listed {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("vast: instance %s was rented for %q and is not in the list: %w", id, name, ctx.Err())
		case <-ticker.C:
		}
	}
}

// newCreateRequest hands the argv over as JSON strings end to end, so no shell ever parses it.
func newCreateRequest(spec provider.Spec) createRequest {
	entrypoint, args := supervised(spec.Command)
	req := createRequest{
		ClientID: "me",
		Image:    spec.Image,
		RunType:  "args",
		Label:    spec.Name,
		Disk:     diskGB(spec),
		Env:      spec.Env,
		OnStart:  entrypoint,
		Args:     args,
	}
	if req.Env == nil {
		req.Env = map[string]string{}
	}
	return req
}

func validateSpec(spec provider.Spec) error {
	if err := spec.Validate(); err != nil {
		return fmt.Errorf("vast: %w", err)
	}
	return nil
}
