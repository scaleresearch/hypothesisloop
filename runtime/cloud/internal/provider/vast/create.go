package vast

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
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
	// ImageLogin is the docker login arguments the image is pulled with, empty for a public one.
	ImageLogin string `json:"image_login,omitempty"`
}

func (p *Provider) Create(ctx context.Context, spec provider.Spec) (string, error) {
	if err := validateSpec(spec); err != nil {
		return "", err
	}
	offers, err := p.candidates(ctx, spec)
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

// rentFirstAvailable rents the first of offers that can still be had. The search index trails
// the market, so an offer it returned after someone took it is passed over: nothing was rented
// and the next one fits the spec as well. When all of them are taken there is no offer, which a
// later Create may find otherwise. Any other refusal ends the Create. The machine rented is
// logged, which is how an operator learns the id of one worth excluding.
func (p *Provider) rentFirstAvailable(ctx context.Context, offers []offer, req createRequest) (string, error) {
	taken := errOfferTaken
	for _, candidate := range offers {
		if !p.held.claim(candidate.ID) {
			continue
		}
		id, err := p.rent(ctx, candidate, req)
		p.held.settle(candidate.ID, id)
		if err == nil {
			log.Printf("vast: rented instance %s for %s on machine %d at $%.4f/h", id, req.Label, candidate.MachineID, candidate.PricePerHour)
			return id, nil
		}
		if !errors.Is(err, errOfferTaken) {
			return "", err
		}
		taken = err
	}
	return "", fmt.Errorf("vast: %w %q: every one of the %d offers tried was taken: %w", provider.ErrNoOffer, req.Label, len(offers), taken)
}

var errOfferTaken = errors.New("offer is no longer available")

// The two refusals of an offer that is taken: the offer is gone from the market, or it is still
// listed and its GPU is rented, which is how the vendor answers for an offer one of the
// account's own instances holds.
var offerTakenMarks = []string{"no_such_ask", "already rented"}

func offerTaken(message string) bool {
	return slices.ContainsFunc(offerTakenMarks, func(mark string) bool { return strings.Contains(message, mark) })
}

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
	case errors.As(err, &failure) && offerTaken(failure.Message):
		return "", fmt.Errorf("%w: %w", errOfferTaken, err)
	case err != nil:
		return "", err
	case !created.Success && offerTaken(created.Message):
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
	if login := spec.ImageLogin; login != nil {
		req.ImageLogin = "-u " + login.Username + " -p " + login.Password + " " + login.Registry
	}
	return req
}

func validateSpec(spec provider.Spec) error {
	if err := spec.Validate(); err != nil {
		return fmt.Errorf("vast: %w", err)
	}
	return nil
}
