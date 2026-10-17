package fly

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Every Machine is the one Machine of an app of its own, created with it on a private network of
// its own and deleted with it. What real Machines taught, and the reason for it:
//
//   - Machines of one app share a private network, whatever the app was created with: one reached
//     another's ports and found every Machine of the app in the app's DNS. Machines of apps on
//     different networks reached nothing of each other. There is no setting that keeps a Machine
//     off its app's network.
//   - A Machine can write its own metadata through its API socket and nothing else of its config.
//     So what the adapter reads back, the instance name and whether a Machine is warm, is kept in
//     the config's env and init, never in metadata.
//
// An instance's id is its app and its Machine id, app/machine.

// appSuffixBytes is the random part of an app's name. App names are unique across the vendor.
const appSuffixBytes = 8

var machineIDPattern = regexp.MustCompile(`^[a-z0-9]+$`)

func randomSuffix() string {
	suffix := make([]byte, appSuffixBytes)
	_, _ = rand.Read(suffix)
	return hex.EncodeToString(suffix)
}

func instanceID(app, machine string) string { return app + "/" + machine }

// splitID refuses an id that names no Machine of an app of this adapter's prefix, so nothing
// outside them is ever read, changed or deleted.
func (p *Provider) splitID(id string) (app, machine string, err error) {
	app, machine, found := strings.Cut(id, "/")
	if !found || !p.ownsApp(app) || !machineIDPattern.MatchString(machine) {
		return "", "", fmt.Errorf("fly: %q is not the id of a machine in an app of prefix %q", id, p.cfg.AppPrefix)
	}
	return app, machine, nil
}

func (p *Provider) ownsApp(app string) bool {
	suffix, found := strings.CutPrefix(app, p.cfg.AppPrefix+"-")
	if !found || len(suffix) != 2*appSuffixBytes {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil && strings.ToLower(suffix) == suffix
}

func (p *Provider) appPath(app string) string {
	return p.machinesURL + "/v1/apps/" + url.PathEscape(app)
}

func (p *Provider) machinePath(app, machine, suffix string) string {
	return p.appPath(app) + "/machines/" + url.PathEscape(machine) + suffix
}

// createApp creates an empty app on a network named after it.
func (p *Provider) createApp(ctx context.Context) (string, error) {
	app := p.cfg.AppPrefix + "-" + p.suffix()
	body := map[string]string{"app_name": app, "org_slug": p.cfg.Org, "network": app}
	if _, err := p.call(ctx, http.MethodPost, p.machinesURL+"/v1/apps", body, nil); err != nil {
		return "", fmt.Errorf("fly: create an app for a machine: %w", err)
	}
	return app, nil
}

// deleteApp deletes the app and its Machine whatever its state. The API answers 404 for an app
// that is gone.
func (p *Provider) deleteApp(ctx context.Context, app string) error {
	_, err := p.call(ctx, http.MethodDelete, p.appPath(app), nil, nil)
	if hasStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

// apps is every app of the organization that this adapter created. The API answers with all of
// them and their count, and an answer that holds fewer than it counts is refused, since an app
// left out would be a Machine left out.
func (p *Provider) apps(ctx context.Context) ([]string, error) {
	var answer struct {
		TotalApps int `json:"total_apps"`
		Apps      []struct {
			Name string `json:"name"`
		} `json:"apps"`
	}
	if _, err := p.call(ctx, http.MethodGet, p.machinesURL+"/v1/apps?"+url.Values{"org_slug": {p.cfg.Org}}.Encode(), nil, &answer); err != nil {
		return nil, err
	}
	if len(answer.Apps) != answer.TotalApps {
		return nil, fmt.Errorf("fly: the app list of organization %q holds %d of the %d apps it counts", p.cfg.Org, len(answer.Apps), answer.TotalApps)
	}
	owned := []string{}
	for _, app := range answer.Apps {
		if p.ownsApp(app.Name) {
			owned = append(owned, app.Name)
		}
	}
	return owned, nil
}
