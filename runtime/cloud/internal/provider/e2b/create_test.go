package e2b

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

const agentTemplate = "hl-70f52c6a3a422ba0352a-2c-4096m"

func agentSpec() provider.Spec {
	return provider.Spec{
		Name:        "hl-agent-pe1-a2",
		Image:       "registry.example/agent:1",
		Command:     []string{"uv", "run", "agent", "--note", `it's "quoted" $HOME`, ""},
		Env:         map[string]string{"HL_TOKEN": "t = 'x' \"y\" $Z", "API_URL": "https://api.example"},
		CPUMillis:   2000,
		MemoryBytes: 4 << 30,
		DiskBytes:   10 << 30,
	}
}

const createdJSON = `{"sandboxID":"sb1","templateID":"tpl1","envdAccessToken":"` + testToken + `","envdVersion":"0.9.0"}`

// creatable wires the calls of a Create that succeeds: the sandbox is made, takes the launch
// file, starts it, shows its state file and is listed.
func creatable(api *fakeAPI, name string) {
	api.on(http.MethodPost, sandboxesPath, answer(201, createdJSON))
	api.on(http.MethodPost, envdFiles, answer(200, `[{"name":"launch","type":"file","path":"/var/tmp/.hl-supervisor/launch"}]`))
	api.on(http.MethodPost, envdStart, stream(`{}`, startedEvent))
	api.on(http.MethodGet, envdFiles, answer(200, "running\n"))
	api.on(http.MethodGet, sandboxesPath, answer(200, sandboxes(sandboxJSON("sb1", name, "running"))))
	api.on(http.MethodDelete, sandboxPath, answer(204, ``))
}

func uploaded(t *testing.T, r recorded) string {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(r.ContentType)
	if err != nil || mediaType != "multipart/form-data" {
		t.Fatalf("upload has content type %q", r.ContentType)
	}
	part, err := multipart.NewReader(strings.NewReader(string(r.Raw)), params["boundary"]).NextPart()
	if err != nil || part.FormName() != "file" {
		t.Fatalf("upload has no part named file: %v", err)
	}
	content, err := io.ReadAll(part)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func TestCreateStartsASandboxOfTheTemplateAndTheCommandInside(t *testing.T) {
	api := newFakeAPI(t)
	creatable(api, "hl-agent-pe1-a2")
	p := newTestProvider(t, api)

	id, err := p.Create(context.Background(), agentSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id != "sb1" {
		t.Fatalf("id = %q, want the sandbox id sb1", id)
	}

	made := api.sent(http.MethodPost, sandboxesPath)
	if len(made) != 1 || made[0].APIKey != testKey {
		t.Fatalf("creates = %+v, want one with the API key", made)
	}
	wantSandbox := map[string]any{
		"templateID": agentTemplate,
		"timeout":    3600.0,
		"autoPause":  false,
		"metadata":   map[string]any{"hl_name": "hl-agent-pe1-a2"},
	}
	if body := made[0].json(t); !reflect.DeepEqual(body, wantSandbox) {
		t.Errorf("sandbox body\n got %v\nwant %v", body, wantSandbox)
	}

	uploads := api.sent(http.MethodPost, envdFiles)
	if len(uploads) != 1 || uploads[0].Query != "path=%2Fvar%2Ftmp%2F.hl-supervisor%2Flaunch" || uploads[0].AccessToken != testToken || uploads[0].APIKey != "" {
		t.Fatalf("uploads = %+v, want the launch file, sent with the sandbox's token and without the API key", uploads)
	}
	wantFile := ". /etc/hl-image.env\n" +
		"export API_URL='https://api.example'\n" +
		`export HL_TOKEN='t = '\''x'\'' "y" $Z'` + "\n" +
		`set -- 'uv' 'run' 'agent' '--note' 'it'\''s "quoted" $HOME' ''` + "\n" + supervisor
	if got := uploaded(t, uploads[0]); got != wantFile {
		t.Errorf("launch file\n got %q\nwant %q", got, wantFile)
	}

	starts := api.sent(http.MethodPost, envdStart)
	if len(starts) != 1 || starts[0].AccessToken != testToken {
		t.Fatalf("starts = %+v, want one with the sandbox's token", starts)
	}
	wantStart := map[string]any{
		"process": map[string]any{"cmd": "/bin/sh", "args": []any{"/var/tmp/.hl-supervisor/launch"}},
		"tag":     "hl-supervisor",
		"stdin":   false,
	}
	if got := sentProcess(t, starts[0]); !reflect.DeepEqual(got, wantStart) {
		t.Errorf("process start\n got %v\nwant %v: envd logs it, so it must hold neither the command nor its environment", got, wantStart)
	}
	if got := len(api.sent(http.MethodDelete, sandboxPath)); got != 0 {
		t.Errorf("the sandbox of a Create that succeeded was destroyed")
	}
}

func TestCreateRefusesBeforeSpendingAnything(t *testing.T) {
	cases := map[string]struct {
		mutate func(*provider.Spec)
		want   string
	}{
		"no name":              {func(s *provider.Spec) { s.Name = "" }, "instance name"},
		"name outside the set": {func(s *provider.Spec) { s.Name = "HL agent" }, "instance name"},
		"no command":           {func(s *provider.Spec) { s.Command = nil }, "no command"},
		"empty executable":     {func(s *provider.Spec) { s.Command = []string{"", "x"} }, "no command"},
		"no image":             {func(s *provider.Spec) { s.Image = "" }, "no image"},
		"no cpu":               {func(s *provider.Spec) { s.CPUMillis = 0 }, "cpu, memory and disk"},
		"no memory":            {func(s *provider.Spec) { s.MemoryBytes = 0 }, "cpu, memory and disk"},
		"no disk":              {func(s *provider.Spec) { s.DiskBytes = 0 }, "cpu, memory and disk"},
		"an accelerator": {func(s *provider.Spec) {
			s.AcceleratorCount, s.AcceleratorType = 1, "nvidia.com/gpu.product=NVIDIA-H100"
		}, "rents none"},
		"negative accelerator":   {func(s *provider.Spec) { s.AcceleratorCount = -1 }, "-1 accelerators"},
		"a shell in an env name": {func(s *provider.Spec) { s.Env = map[string]string{"A=1; rm -rf /; B": "1"} }, "not an environment variable name"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			p := newTestProvider(t, api)
			spec := agentSpec()
			tc.mutate(&spec)

			_, err := p.Create(context.Background(), spec)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one about %q", err, tc.want)
			}
			if api.total() != 0 {
				t.Errorf("%d requests were sent for a spec the adapter cannot meet", api.total())
			}
		})
	}
}

func TestCreateNamesTheTemplateThatIsNotBuiltAndHowToBuildIt(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodPost, sandboxesPath, answer(404, `{"code":404,"message":"template '`+agentTemplate+`' not found"}`))
	p := newTestProvider(t, api)

	_, err := p.Create(context.Background(), agentSpec())
	want := "build it with `infra/images/e2b-template.sh registry.example/agent:1 2 4096`"
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), agentTemplate) {
		t.Fatalf("err = %v, want one naming template %s and saying %q", err, agentTemplate, want)
	}
	if api.total() != 1 {
		t.Errorf("requests = %d, want the one create", api.total())
	}
}

func TestCreateReportsARefusalAndLeavesNothing(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"lifetime above the plan's cap": {400, `{"code":400,"message":"Timeout cannot be greater than 1 hours"}`},
		"concurrent sandbox limit":      {429, `{"code":429,"message":"you have reached the maximum number of concurrent sandboxes"}`},
		"server error":                  {500, `{"code":500,"message":"oops"}`},
		"no capacity":                   {503, `{"code":503,"message":"no nodes available"}`},
		"bad key":                       {401, `{"code":401,"message":"invalid API key"}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on(http.MethodPost, sandboxesPath, answer(tc.status, tc.body))
			p := newTestProvider(t, api)

			_, err := p.Create(context.Background(), agentSpec())
			if err == nil || !strings.Contains(err.Error(), tc.body) {
				t.Fatalf("err = %v, want one carrying the vendor's answer", err)
			}
			if api.total() != 1 {
				t.Errorf("requests = %d, want the one create and no retry", api.total())
			}
		})
	}
}

func TestCreateWithoutAnIDOrATokenIsAnError(t *testing.T) {
	for name, body := range map[string]string{
		"no id":    `{"envdAccessToken":"` + testToken + `"}`,
		"no token": `{"sandboxID":"sb1"}`,
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on(http.MethodPost, sandboxesPath, answer(201, body))
			p := newTestProvider(t, api)

			if _, err := p.Create(context.Background(), agentSpec()); err == nil || !strings.Contains(err.Error(), "without an id or without its token") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestCreateDestroysASandboxWhoseCommandItCouldNotStart(t *testing.T) {
	cases := map[string]struct {
		break_ func(*fakeAPI)
		want   string
	}{
		"the upload fails": {func(a *fakeAPI) {
			a.on(http.MethodPost, envdFiles, answer(507, `{"code":507,"message":"not enough disk space"}`))
		},
			"could not be uploaded"},
		"envd is not up": {func(a *fakeAPI) { a.on(http.MethodPost, envdFiles, answer(502, deadEnvd)) },
			"could not be uploaded"},
		"the start is refused": {func(a *fakeAPI) { a.on(http.MethodPost, envdStart, answer(401, `{"code":"unauthenticated"}`)) },
			"could not be started"},
		"the start fails in the stream": {func(a *fakeAPI) {
			a.on(http.MethodPost, envdStart, stream(`{"error":{"code":"invalid_argument","message":"error starting process: fork/exec /bin/sh: no such file or directory"}}`))
		}, "fork/exec /bin/sh"},
		"the stream breaks off": {func(a *fakeAPI) { a.on(http.MethodPost, envdStart, answer(200, "\x00\x00\x00")) },
			"broke off"},
		"the state cannot be read": {func(a *fakeAPI) { a.on(http.MethodGet, envdFiles, answer(500, `oops`)) },
			"could not be seen starting"},
		"the list fails": {func(a *fakeAPI) { a.on(http.MethodGet, sandboxesPath, answer(500, `oops`)) },
			"could not be confirmed in the list"},
		"the template is smaller than its name": {func(a *fakeAPI) {
			a.on(http.MethodGet, sandboxesPath, answer(200, strings.Replace(sandboxes(sandboxJSON("sb1", "hl-agent-pe1-a2", "running")), `"memoryMB":4096`, `"memoryMB":2048`, 1)))
		}, "2048 MiB of memory"},
		"the template has too little disk": {func(a *fakeAPI) {
			a.on(http.MethodGet, sandboxesPath, answer(200, strings.Replace(sandboxes(sandboxJSON("sb1", "hl-agent-pe1-a2", "running")), `"diskSizeMB":11484`, `"diskSizeMB":5000`, 1)))
		}, "5000 MiB of disk"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			creatable(api, "hl-agent-pe1-a2")
			tc.break_(api)
			p := newTestProvider(t, api)

			_, err := p.Create(context.Background(), agentSpec())
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "it was destroyed") {
				t.Fatalf("err = %v, want one about %q that says the sandbox was destroyed", err, tc.want)
			}
			if got := api.sent(http.MethodDelete, sandboxPath); len(got) != 1 || got[0].APIKey != testKey {
				t.Errorf("destroys = %+v, want one", got)
			}
		})
	}
}

func TestCreateGivesUpOnASupervisorThatNeverWritesItsState(t *testing.T) {
	api := newFakeAPI(t)
	creatable(api, "hl-agent-pe1-a2")
	api.on(http.MethodGet, envdFiles, answer(404, noFileJSON))
	p := newTestProvider(t, api)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := p.Create(ctx, agentSpec())
	if err == nil || !strings.Contains(err.Error(), "its supervisor did not start") || !strings.Contains(err.Error(), "/etc/hl-image.env") {
		t.Fatalf("err = %v, want one saying the supervisor did not start and what it needs", err)
	}
	if got := len(api.sent(http.MethodDelete, sandboxPath)); got != 1 {
		t.Errorf("destroys = %d, want 1: the caller's context ended and the sandbox must still go", got)
	}
}

func TestCreateIsNotConfirmedByAnotherSandboxOfTheSameName(t *testing.T) {
	api := newFakeAPI(t)
	creatable(api, "hl-agent-pe1-a2")
	api.on(http.MethodGet, sandboxesPath, inSequence(
		answer(200, sandboxes(sandboxJSON("older", "hl-agent-pe1-a2", "running"))),
		answer(200, sandboxes(sandboxJSON("older", "hl-agent-pe1-a2", "running"), sandboxJSON("sb1", "hl-agent-pe1-a2", "running"))),
	))
	p := newTestProvider(t, api)

	if id, err := p.Create(context.Background(), agentSpec()); err != nil || id != "sb1" {
		t.Fatalf("Create = %q, %v", id, err)
	}
	if got := len(api.sent(http.MethodGet, sandboxesPath)); got != 2 {
		t.Errorf("lists = %d, want 2: the first showed only the older sandbox of the name", got)
	}
}

func TestCreateSaysWhenTheSandboxItCouldNotFinishMayStillBeBilled(t *testing.T) {
	api := newFakeAPI(t)
	creatable(api, "hl-agent-pe1-a2")
	api.on(http.MethodPost, envdFiles, answer(500, `oops`))
	api.on(http.MethodDelete, sandboxPath, answer(500, `oops`))
	p := newTestProvider(t, api)

	_, err := p.Create(context.Background(), agentSpec())
	if err == nil || !strings.Contains(err.Error(), "may still be billed") || !strings.Contains(err.Error(), "sb1") {
		t.Fatalf("err = %v, want one naming the sandbox that may still be billed", err)
	}
}
