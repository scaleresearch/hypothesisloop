package runpod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider/vendorapi"
)

// listedTimeout bounds the wait for a created pod to show in List, and for one on the global
// network to show its address.
const listedTimeout = 30 * time.Second

// noCapacity is in the detail of the 400 the API answers a create with when none of the asked
// data centers has a machine for it right now. The API gives capacity no code of its own.
const noCapacity = "no longer any instances available"

type gpuRequest struct {
	ID                 string `json:"id"`
	Count              int    `json:"count"`
	MinVCPUCountPerGPU int64  `json:"minVcpuCountPerGpu"`
	MinRAMPerGPU       int64  `json:"minRamPerGpu"`
}

type cpuRequest struct {
	ID        string `json:"id"`
	VCPUCount int    `json:"vcpuCount"`
}

type createRequest struct {
	Name             string            `json:"name"`
	Image            string            `json:"image"`
	Cloud            string            `json:"cloud"`
	GPU              *gpuRequest       `json:"gpu,omitempty"`
	CPU              *cpuRequest       `json:"cpu,omitempty"`
	Disk             int64             `json:"disk"`
	DataCenterIDs    []string          `json:"dataCenterIds"`
	GlobalNetworking bool              `json:"globalNetworking,omitempty"`
	Entrypoint       []string          `json:"entrypoint"`
	Cmd              []string          `json:"cmd"`
	Env              map[string]string `json:"env"`
}

func (p *Provider) Create(ctx context.Context, spec provider.Spec) (string, error) {
	if err := validateSpec(spec); err != nil {
		return "", err
	}
	offers, why, err := p.offers(ctx, spec)
	if err != nil {
		return "", err
	}
	if spec.Place != "" {
		if why = networkable(spec); why == "" {
			offers = inPlace(offers, spec.Place)
			why = "Runpod has no stock of the shape in " + spec.Place
		} else {
			offers = nil
		}
	}
	if len(offers) == 0 {
		return "", fmt.Errorf("runpod: %w %q: %s", provider.ErrNoOffer, spec.Name, why)
	}
	created, err := p.rentFirstAvailable(ctx, spec, offers)
	if err != nil {
		return "", err
	}
	if err := p.awaitListed(ctx, created, spec); err != nil {
		destroy := func(ctx context.Context) error { return p.Destroy(ctx, created.ID) }
		return "", vendorapi.GiveBack(ctx, destroy, requestTimeout, err)
	}
	return created.ID, nil
}

// inPlace is offers in place alone.
func inPlace(offers []offer, place string) []offer {
	var kept []offer
	for _, o := range offers {
		for _, dc := range o.dataCenters {
			if dc.ID == place {
				o.dataCenters = []stock{dc}
				kept = append(kept, o)
				break
			}
		}
	}
	return kept
}

// rentFirstAvailable asks for the first of offers in all of its data centers, and for the next
// one when the vendor has no machine for it after all. Any other refusal ends the Create.
func (p *Provider) rentFirstAvailable(ctx context.Context, spec provider.Spec, offers []offer) (pod, error) {
	var refusals []string
	for _, o := range offers {
		var created pod
		err := p.call(ctx, http.MethodPost, "/v2/pods", p.createRequest(spec, o), &created)
		var failure *statusError
		if errors.As(err, &failure) && failure.Status == http.StatusBadRequest && strings.Contains(failure.Detail, noCapacity) {
			refusals = append(refusals, failure.Detail)
			continue
		}
		if err != nil {
			return pod{}, err
		}
		if created.ID == "" {
			return pod{}, fmt.Errorf("runpod: the create of %q answered with no pod id", spec.Name)
		}
		p.log("rented pod %s for %s as %s in %s at $%.4f/h", created.ID, spec.Name, o.kind(), text(created.DataCenterID), created.Cost)
		return created, nil
	}
	return pod{}, fmt.Errorf("runpod: %w %q: every one of the %d offers tried had no machine: %s", provider.ErrNoOffer, spec.Name, len(offers), strings.Join(refusals, "; "))
}

func (o offer) kind() string {
	if o.gpu != "" {
		return fmt.Sprintf("%d of %s", o.gpus, o.gpu)
	}
	return fmt.Sprintf("%s with %d vCPUs", o.cpu, o.vcpus)
}

func text(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// createRequest hands the argv over as a JSON array, entrypoint first, so no shell parses it.
func (p *Provider) createRequest(spec provider.Spec, o offer) createRequest {
	command := supervisedCommand(spec.Name, spec.Command, spec.Place != "")
	req := createRequest{
		Name:             spec.Name,
		Image:            spec.Image,
		Cloud:            "SECURE",
		Disk:             provider.CeilDiv(spec.DiskBytes, gib),
		GlobalNetworking: spec.Place != "",
		Entrypoint:       command[:1],
		Cmd:              command[1:],
		Env:              spec.Env,
	}
	if req.Env == nil {
		req.Env = map[string]string{}
	}
	for _, dc := range o.dataCenters {
		req.DataCenterIDs = append(req.DataCenterIDs, dc.ID)
	}
	if o.gpu != "" {
		req.Cloud = p.cfg.Cloud
		count := int64(o.gpus)
		req.GPU = &gpuRequest{
			ID: o.gpu, Count: o.gpus,
			MinVCPUCountPerGPU: max(1, provider.CeilDiv(provider.CeilDiv(spec.CPUMillis, 1000), count)),
			MinRAMPerGPU:       max(1, provider.CeilDiv(provider.CeilDiv(spec.MemoryBytes, gib), count)),
		}
	} else {
		req.CPU = &cpuRequest{ID: o.cpu, VCPUCount: o.vcpus}
	}
	return req
}

// awaitListed holds Create until the pod is in List, which is what tells a caller with no memory
// of this call that the name is taken, and until one on the global network shows its address.
func (p *Provider) awaitListed(ctx context.Context, created pod, spec provider.Spec) error {
	ctx, cancel := context.WithTimeout(ctx, listedTimeout)
	defer cancel()
	ticker := time.NewTicker(p.poll)
	defer ticker.Stop()
	for {
		listed, err := p.List(ctx, spec.Name)
		if err != nil {
			return fmt.Errorf("runpod: pod %s was rented for %q and could not be confirmed in the list: %w", created.ID, spec.Name, err)
		}
		for _, i := range listed {
			if i.ID == created.ID && i.Name == spec.Name && (spec.Place == "" || i.PrivateAddress != "") {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("runpod: pod %s was rented for %q and is not in the list with what it needs: %w", created.ID, spec.Name, ctx.Err())
		case <-ticker.C:
		}
	}
}

func validateSpec(spec provider.Spec) error {
	if err := spec.Validate(); err != nil {
		return fmt.Errorf("runpod: %w", err)
	}
	if spec.Architecture != architecture {
		return fmt.Errorf("runpod: spec %q runs on %s, and this adapter rents %s machines only", spec.Name, spec.Architecture, architecture)
	}
	if spec.ImageLogin != nil {
		return fmt.Errorf("runpod: spec %q pulls its image with a login, and this adapter pulls only public images", spec.Name)
	}
	if len(spec.AvoidHosts) > 0 {
		return fmt.Errorf("runpod: spec %q avoids hosts, and Runpod names none", spec.Name)
	}
	return nil
}
