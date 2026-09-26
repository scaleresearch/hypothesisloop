package e2b

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// TemplateBuilder builds the template an image and size need. E2B boots from a template, and a
// template fixes cores and memory, so there is one per image and size, under the name Create asks
// for (templateName).
//
// E2B takes the image's files and drops its config, so the image's ENV, WORKDIR and USER are read
// from the registry and added as build steps, which needs base64 in the image. ENTRYPOINT and CMD
// are not needed: the adapter starts the command it is given.
//
// The image tag is part of the name and its content is not: after pushing new content under an
// old tag, build the template again.
type TemplateBuilder struct {
	// APIKey is the team's key. RegistryUsername and RegistryPassword are set for an image in a
	// private registry: both this builder and E2B pull with them.
	APIKey           string
	RegistryUsername string
	RegistryPassword string
	// Out receives the build's log.
	Out io.Writer

	e2bURL         string
	registryScheme string
	client         *http.Client
	pollEvery      time.Duration
}

func (b TemplateBuilder) withDefaults() TemplateBuilder {
	if b.e2bURL == "" {
		b.e2bURL = "https://api.e2b.app"
	}
	if b.registryScheme == "" {
		b.registryScheme = "https"
	}
	if b.client == nil {
		b.client = &http.Client{Timeout: 60 * time.Second}
	}
	if b.pollEvery == 0 {
		b.pollEvery = 5 * time.Second
	}
	return b
}

// reference is an image reference taken apart the way a container runtime does.
type imageReference struct {
	registry, repository, reference string
}

func parseReference(image string) imageReference {
	ref := imageReference{registry: "registry-1.docker.io", reference: "latest"}
	rest := image
	if first, after, hasSlash := strings.Cut(image, "/"); !hasSlash {
		rest = "library/" + image
	} else if strings.ContainsAny(first, ".:") || first == "localhost" {
		ref.registry, rest = first, after
	}
	if repository, digest, pinned := strings.Cut(rest, "@"); pinned {
		ref.repository, ref.reference = repository, digest
	} else if colon := strings.LastIndex(rest, ":"); colon >= 0 {
		ref.repository, ref.reference = rest[:colon], rest[colon+1:]
	} else {
		ref.repository = rest
	}
	return ref
}

// imageConfig is what E2B drops of an image.
type imageConfig struct {
	Config struct {
		Env        []string `json:"Env"`
		WorkingDir string   `json:"WorkingDir"`
		User       string   `json:"User"`
	} `json:"config"`
}

type buildStep struct {
	Type string   `json:"type"`
	Args []string `json:"args"`
}

var variableName = regexp.MustCompile(`\A[A-Za-z_][A-Za-z0-9_]*\z`)

// steps are the build steps that give a template what the image's config said. A template keeps
// an ENV step for the build only, so the image's environment goes into a file the adapter's
// supervisor reads before it sets the command's own (supervise.go). Every value is in single
// quotes, inside which a shell reads each byte as itself. USER comes last: the file and the
// working directory are made by root.
func buildSteps(image string, config imageConfig) ([]buildStep, error) {
	var file strings.Builder
	for _, entry := range config.Config.Env {
		name, value, _ := strings.Cut(entry, "=")
		// A name is written into the file unquoted, and one that is not a variable name would be
		// read by the shell of every sandbox as a command.
		if !variableName.MatchString(name) {
			return nil, fmt.Errorf("%s sets %q in its ENV, which is not an environment variable name", image, name)
		}
		fmt.Fprintf(&file, "export %s='%s'\n", name, strings.ReplaceAll(value, "'", `'\''`))
	}
	if file.Len() == 0 {
		file.WriteString("\n")
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(file.String()))
	built := []buildStep{{"RUN", []string{"echo " + encoded + " | base64 -d > " + imageEnvPath + " && chmod 644 " + imageEnvPath, "root"}}}
	if config.Config.WorkingDir != "" {
		built = append(built, buildStep{"WORKDIR", []string{config.Config.WorkingDir}})
	}
	if config.Config.User != "" {
		built = append(built, buildStep{"USER", []string{config.Config.User}})
	}
	return built, nil
}

func (b TemplateBuilder) get(address string, header http.Header, into any) (*http.Response, error) {
	request, err := http.NewRequest(http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	request.Header = header
	response, err := b.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if into == nil {
		return response, nil
	}
	if response.StatusCode != http.StatusOK {
		return response, fmt.Errorf("GET %s answered %d", address, response.StatusCode)
	}
	return response, json.NewDecoder(response.Body).Decode(into)
}

var challengeField = regexp.MustCompile(`(realm|service)="([^"]*)"`)

// registryAuthorization is the header a registry wants. A registry answers its first request
// with the address of the token service to ask.
func (b TemplateBuilder) registryAuthorization(ref imageReference) (string, error) {
	basic := ""
	if b.RegistryUsername != "" {
		if b.RegistryPassword == "" {
			return "", fmt.Errorf("E2B_REGISTRY_PASSWORD must be set with E2B_REGISTRY_USERNAME")
		}
		basic = "Basic " + base64.StdEncoding.EncodeToString([]byte(b.RegistryUsername+":"+b.RegistryPassword))
	}
	response, err := b.get(b.registryScheme+"://"+ref.registry+"/v2/", http.Header{}, nil)
	if err != nil {
		return "", err
	}
	challenge := response.Header.Get("WWW-Authenticate")
	switch {
	case strings.HasPrefix(strings.ToLower(challenge), "bearer"):
		fields := map[string]string{}
		for _, match := range challengeField.FindAllStringSubmatch(challenge, -1) {
			fields[match[1]] = match[2]
		}
		header := http.Header{}
		if basic != "" {
			header.Set("Authorization", basic)
		}
		var token struct {
			Token       string `json:"token"`
			AccessToken string `json:"access_token"`
		}
		query := url.Values{"service": {fields["service"]}, "scope": {"repository:" + ref.repository + ":pull"}}
		if _, err := b.get(fields["realm"]+"?"+query.Encode(), header, &token); err != nil {
			return "", fmt.Errorf("the registry's token service: %w", err)
		}
		if token.Token == "" {
			token.Token = token.AccessToken
		}
		return "Bearer " + token.Token, nil
	case strings.HasPrefix(strings.ToLower(challenge), "basic"):
		return basic, nil
	}
	return "", nil
}

const manifestTypes = "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"

// imageConfigOf reads the config of the image's linux/amd64 variant, which is what E2B runs.
func (b TemplateBuilder) imageConfigOf(image string) (imageConfig, error) {
	ref := parseReference(image)
	authorization, err := b.registryAuthorization(ref)
	if err != nil {
		return imageConfig{}, err
	}
	fetch := func(path string, into any) error {
		header := http.Header{"Accept": {manifestTypes}}
		if authorization != "" {
			header.Set("Authorization", authorization)
		}
		_, err := b.get(b.registryScheme+"://"+ref.registry+"/v2/"+ref.repository+"/"+path, header, into)
		return err
	}
	var manifest struct {
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := fetch("manifests/"+ref.reference, &manifest); err != nil {
		return imageConfig{}, err
	}
	if len(manifest.Manifests) > 0 {
		digest := ""
		for _, variant := range manifest.Manifests {
			if variant.Platform.OS == "linux" && variant.Platform.Architecture == "amd64" {
				digest = variant.Digest
				break
			}
		}
		if digest == "" {
			return imageConfig{}, fmt.Errorf("%s has no linux/amd64 image, which is what E2B runs", image)
		}
		manifest.Config.Digest = ""
		if err := fetch("manifests/"+digest, &manifest); err != nil {
			return imageConfig{}, err
		}
	}
	var config imageConfig
	return config, fetch("blobs/"+manifest.Config.Digest, &config)
}

// e2b sends one request to E2B. The answer's body is in the error of a refused request: it says
// why, and holds no secret.
func (b TemplateBuilder) e2b(method, path string, body, into any) error {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, b.e2bURL+path, payload)
	if err != nil {
		return err
	}
	request.Header.Set("X-API-Key", b.APIKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := b.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%s %s answered %d: %s", method, path, response.StatusCode, strings.TrimSpace(string(answer)))
	}
	if into == nil {
		return nil
	}
	return json.Unmarshal(answer, into)
}

// Build builds the template of image at cores and memoryMiB and waits until it is ready.
func (b TemplateBuilder) Build(image string, cores, memoryMiB int64) error {
	b = b.withDefaults()
	if b.APIKey == "" {
		return fmt.Errorf("E2B_API_KEY must hold the team API key")
	}
	config, err := b.imageConfigOf(image)
	if err != nil {
		return err
	}
	built, err := buildSteps(image, config)
	if err != nil {
		return err
	}
	name := templateName(image, size{CPUs: cores, MemoryMB: memoryMiB})
	kinds := make([]string, len(built))
	for i, s := range built {
		kinds[i] = s.Type
	}
	fmt.Fprintf(b.Out, "==> template %s from %s: %d cores, %d MiB\n    steps taken from the image: %s\n", name, image, cores, memoryMiB, strings.Join(kinds, ", "))

	var created struct {
		TemplateID string `json:"templateID"`
		BuildID    string `json:"buildID"`
	}
	if err := b.e2b(http.MethodPost, "/v3/templates", map[string]any{"name": name, "cpuCount": cores, "memoryMB": memoryMiB}, &created); err != nil {
		return err
	}
	start := map[string]any{"fromImage": image, "steps": built}
	if b.RegistryUsername != "" {
		start["fromImageRegistry"] = map[string]string{"type": "registry", "username": b.RegistryUsername, "password": b.RegistryPassword}
	}
	buildPath := "/templates/" + created.TemplateID + "/builds/" + created.BuildID
	if err := b.e2b(http.MethodPost, "/v2"+buildPath, start, nil); err != nil {
		return err
	}
	for offset := 0; ; {
		var status struct {
			Status     string `json:"status"`
			LogEntries []struct {
				Message string `json:"message"`
			} `json:"logEntries"`
			Reason struct {
				Message string `json:"message"`
			} `json:"reason"`
		}
		if err := b.e2b(http.MethodGet, fmt.Sprintf("%s/status?logsOffset=%d&limit=100", buildPath, offset), nil, &status); err != nil {
			return err
		}
		for _, entry := range status.LogEntries {
			fmt.Fprintf(b.Out, "    %s\n", strings.ReplaceAll(entry.Message, "\r", ""))
		}
		offset += len(status.LogEntries)
		switch status.Status {
		case "ready":
			fmt.Fprintf(b.Out, "e2b-template: %s is ready (template %s)\n", name, created.TemplateID)
			return nil
		case "error":
			reason := status.Reason.Message
			if reason == "" {
				reason = "no reason given"
			}
			return fmt.Errorf("the build of %s failed: %s", name, reason)
		}
		time.Sleep(b.pollEvery)
	}
}

// TemplateOf names one template: an image at a size.
type TemplateOf struct {
	Image     string
	Cores     int64
	MemoryMiB int64
}

// ConformanceTemplate is the template the adapter's conformance tests start their sandboxes
// from. It belongs to no publish, so Prune always keeps it.
var ConformanceTemplate = TemplateOf{Image: "alpine/curl:8.14.1", Cores: 1, MemoryMiB: 1024}

// Prune deletes every template built here other than the ones of keep and the conformance
// tests'. Each image tag gets a template of its own, so without this every publish leaves one
// behind. A template under a name this builder does not give is left alone.
func (b TemplateBuilder) Prune(keep []TemplateOf) error {
	b = b.withDefaults()
	if b.APIKey == "" {
		return fmt.Errorf("E2B_API_KEY must hold the team API key")
	}
	if len(keep) == 0 {
		return fmt.Errorf("no template to keep was named, and the one agents start from is never deleted")
	}
	kept := map[string]bool{}
	for _, t := range append([]TemplateOf{ConformanceTemplate}, keep...) {
		kept[templateName(t.Image, size{CPUs: t.Cores, MemoryMB: t.MemoryMiB})] = true
	}
	var templates []struct {
		TemplateID string   `json:"templateID"`
		Aliases    []string `json:"aliases"`
	}
	if err := b.e2b(http.MethodGet, "/templates", nil, &templates); err != nil {
		return err
	}
	for _, t := range templates {
		ours, keeping := false, false
		for _, alias := range t.Aliases {
			ours = ours || strings.HasPrefix(alias, templatePrefix)
			keeping = keeping || kept[alias]
		}
		if !ours || keeping {
			continue
		}
		if err := b.e2b(http.MethodDelete, "/templates/"+url.PathEscape(t.TemplateID), nil, nil); err != nil {
			return err
		}
		fmt.Fprintf(b.Out, "e2b-template: deleted %s (template %s)\n", strings.Join(t.Aliases, ", "), t.TemplateID)
	}
	return nil
}
