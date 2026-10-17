package e2b

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
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

// creatable wires the calls of a Create that succeeds: the sandbox is made and listed, takes the
// directory, the launch file and the start, and shows its state file.
func creatable(api *fakeAPI, name string) {
	api.on(http.MethodPost, sandboxesPath, answer(201, createdJSON))
	api.on(http.MethodGet, sandboxesPath, answer(200, sandboxes(sandboxJSON("sb1", name, "running"))))
	api.on(http.MethodPost, envdStart, stream(`{}`, startedEvent, endedEvent))
	api.on(http.MethodPost, envdFiles, answer(200, `[{"name":"launch","type":"file","path":"/var/tmp/.hl-supervisor/launch"}]`))
	api.on(http.MethodGet, envdFiles, answer(200, "running\n"))
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
		"network":    map[string]any{"allowPublicTraffic": false},
	}
	if body := made[0].json(t); !reflect.DeepEqual(body, wantSandbox) {
		t.Errorf("sandbox body\n got %v\nwant %v", body, wantSandbox)
	}

	uploads := api.sent(http.MethodPost, envdFiles)
	if len(uploads) != 1 || uploads[0].Query != "path=%2Fvar%2Ftmp%2F.hl-supervisor%2Flaunch" || uploads[0].AccessToken != testToken || uploads[0].APIKey != "" {
		t.Fatalf("uploads = %+v, want the launch file, sent with the sandbox's token and without the API key", uploads)
	}
	if got := uploaded(t, uploads[0]); got != launchFile(agentSpec().Command, agentSpec().Env, 55*time.Minute) {
		t.Errorf("launch file = %q", got)
	}

	starts := api.sent(http.MethodPost, envdStart)
	if len(starts) != 2 || starts[0].AccessToken != testToken || starts[1].AccessToken != testToken {
		t.Fatalf("starts = %+v, want the directory and the launch, each with the sandbox's token", starts)
	}
	wantDirectory := map[string]any{
		"process": map[string]any{"cmd": "mkdir", "args": []any{"-m", "700", "/var/tmp/.hl-supervisor"}},
		"stdin":   false,
	}
	if got := sentProcess(t, starts[0]); !reflect.DeepEqual(got, wantDirectory) {
		t.Errorf("first process start\n got %v\nwant %v: the launch file must land where no other user reads it", got, wantDirectory)
	}
	wantStart := map[string]any{
		"process": map[string]any{"cmd": "/bin/sh", "args": []any{"/var/tmp/.hl-supervisor/launch"}},
		"tag":     "hl-supervisor",
		"stdin":   false,
	}
	if got := sentProcess(t, starts[1]); !reflect.DeepEqual(got, wantStart) {
		t.Errorf("process start\n got %v\nwant %v: envd logs it, so it must hold neither the command nor its environment", got, wantStart)
	}
	if got := len(api.sent(http.MethodDelete, sandboxPath)); got != 0 {
		t.Errorf("the sandbox of a Create that succeeded was destroyed")
	}
}

// A sandbox is rented for the plan's longest lifetime, and its supervisor ends the command five
// minutes before the vendor ends the sandbox and says that it did.
func TestCreateRentsTheSandboxForThePlansLongestAndTheSupervisorEndsTheCommandFiveMinutesBefore(t *testing.T) {
	api := newFakeAPI(t)
	creatable(api, "hl-agent-pe1-a2")
	p := newTestProvider(t, api)
	if _, err := p.Create(context.Background(), agentSpec()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := api.sent(http.MethodPost, sandboxesPath)[0].json(t)["timeout"]; got != float64(3600) {
		t.Errorf("timeout = %v, want 3600", got)
	}
	file := uploaded(t, api.sent(http.MethodPost, envdFiles)[0])
	if !strings.Contains(file, "(sleep 3300 && kill -USR1 $$)") || !strings.Contains(file, "echo expired") {
		t.Errorf("launch file, want it to count 3300 seconds and to say expired:\n%s", file)
	}
}

func TestCreateSeesTheSandboxListedBeforeAnythingOfTheSpecEntersIt(t *testing.T) {
	api := newFakeAPI(t)
	creatable(api, "hl-agent-pe1-a2")
	p := newTestProvider(t, api)

	if _, err := p.Create(context.Background(), agentSpec()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := []string{
		"POST " + sandboxesPath, "GET " + sandboxesPath,
		"POST " + envdStart, "POST " + envdFiles, "POST " + envdStart, "GET " + envdFiles,
	}
	if got := api.order(); !slices.Equal(got, want) {
		t.Errorf("requests\n got %q\nwant %q", got, want)
	}
}

func TestCreateRefusesBeforeSpendingAnything(t *testing.T) {
	cases := map[string]struct {
		mutate func(*provider.Spec)
		want   string
	}{
		"no name":    {func(s *provider.Spec) { s.Name = "" }, "instance name"},
		"no command": {func(s *provider.Spec) { s.Command = nil }, "no command"},
		"no image":   {func(s *provider.Spec) { s.Image = "" }, "no image"},
		"no disk":    {func(s *provider.Spec) { s.DiskBytes = 0 }, "cpu, memory and disk"},
		"an accelerator": {func(s *provider.Spec) {
			s.AcceleratorCount, s.AcceleratorType = 1, "nvidia.com/gpu.product=NVIDIA-H100"
		}, "rents none"},
		"negative accelerator": {func(s *provider.Spec) { s.AcceleratorCount = -1 }, "-1 accelerators"},
		"image login": {func(s *provider.Spec) {
			s.ImageLogin = &provider.ImageLogin{Registry: "r", Username: "u", Password: "p"}
		}, "only public images"},
		"a shell in an env name": {func(s *provider.Spec) { s.Env = map[string]string{"A=1; rm -rf /; B": "1"} }, "not an environment variable name"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			p := newTestProvider(t, api)
			spec := agentSpec()
			tc.mutate(&spec)

			_, err := p.Create(context.Background(), spec)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.HasPrefix(err.Error(), "e2b: ") {
				t.Fatalf("err = %v, want one of e2b about %q", err, tc.want)
			}
			if api.total() != 0 {
				t.Errorf("%d requests were sent for a spec the adapter cannot meet", api.total())
			}
		})
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

// The size a template really gives is only in the list. A command started before that is read
// has its tokens in a sandbox that is about to be destroyed, on every pass.
func TestCreateStartsNothingInASandboxSmallerThanTheSpec(t *testing.T) {
	listed := sandboxes(sandboxJSON("sb1", "hl-agent-pe1-a2", "running"))
	cases := map[string]struct {
		listed string
		want   string
	}{
		"too little disk":   {strings.Replace(listed, `"diskSizeMB":11484`, `"diskSizeMB":5000`, 1), "5000 MiB of disk"},
		"too little memory": {strings.Replace(listed, `"memoryMB":4096`, `"memoryMB":2048`, 1), "2048 MiB of memory"},
		"too few cores":     {strings.Replace(listed, `"cpuCount":2`, `"cpuCount":1`, 1), "1 cores"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			creatable(api, "hl-agent-pe1-a2")
			api.on(http.MethodGet, sandboxesPath, answer(200, tc.listed))
			p := newTestProvider(t, api)

			_, err := p.Create(context.Background(), agentSpec())
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "it was destroyed") {
				t.Fatalf("err = %v, want one about %q that says the sandbox was destroyed", err, tc.want)
			}
			want := []string{"POST " + sandboxesPath, "GET " + sandboxesPath, "DELETE " + sandboxPath}
			if got := api.order(); !slices.Equal(got, want) {
				t.Errorf("requests = %q, want %q: nothing may be written or started in the sandbox", got, want)
			}
		})
	}
}

func TestCreateDestroysASandboxWhoseCommandItCouldNotStart(t *testing.T) {
	failedEvent := `{"event":{"end":{"exited":true,"status":"exit status 1"}}}`
	cases := map[string]struct {
		break_ func(*fakeAPI)
		want   string
	}{
		"the directory cannot be made": {func(a *fakeAPI) {
			a.on(http.MethodPost, envdStart, stream(`{}`, startedEvent, stderrEvent("mkdir: can't create directory: Read-only file system\n"), failedEvent))
		}, "Read-only file system"},
		"envd is not up": {func(a *fakeAPI) { a.on(http.MethodPost, envdStart, answer(502, deadEnvd)) },
			"directory could not be made"},
		"the upload fails": {func(a *fakeAPI) {
			a.on(http.MethodPost, envdFiles, answer(507, `{"code":507,"message":"not enough disk space"}`))
		}, "could not be uploaded"},
		"the start is refused": {func(a *fakeAPI) {
			a.on(http.MethodPost, envdStart, inSequence(stream(`{}`, startedEvent, endedEvent), answer(401, `{"code":"unauthenticated"}`)))
		}, "could not be started"},
		"the start fails in the stream": {func(a *fakeAPI) {
			a.on(http.MethodPost, envdStart, inSequence(stream(`{}`, startedEvent, endedEvent),
				stream(`{"error":{"code":"invalid_argument","message":"error starting process: fork/exec /bin/sh: no such file or directory"}}`)))
		}, "fork/exec /bin/sh"},
		"the stream breaks off": {func(a *fakeAPI) {
			a.on(http.MethodPost, envdStart, inSequence(stream(`{}`, startedEvent, endedEvent), answer(200, "\x00\x00\x00")))
		}, "broke off"},
		"the state cannot be read": {func(a *fakeAPI) { a.on(http.MethodGet, envdFiles, answer(500, `oops`)) },
			"could not be seen starting"},
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

func TestCreateReadsTheListAgainAfterAListThatFailed(t *testing.T) {
	api := newFakeAPI(t)
	creatable(api, "hl-agent-pe1-a2")
	refusal := answer(429, `{"code":429,"message":"rate limit exceeded"}`)
	api.on(http.MethodGet, sandboxesPath, inSequence(refusal, answer(500, `oops`), answer(200, sandboxes(sandboxJSON("sb1", "hl-agent-pe1-a2", "running")))))
	p := newTestProvider(t, api)

	if id, err := p.Create(context.Background(), agentSpec()); err != nil || id != "sb1" {
		t.Fatalf("Create = %q, %v, want the sandbox: one refused list says nothing about it", id, err)
	}
	if got := len(api.sent(http.MethodDelete, sandboxPath)); got != 0 {
		t.Errorf("destroys = %d: a sandbox that exists was destroyed over a list that failed", got)
	}
}

func TestCreateDestroysASandboxItCouldNotConfirmInTheWholeWindow(t *testing.T) {
	api := newFakeAPI(t)
	creatable(api, "hl-agent-pe1-a2")
	api.on(http.MethodGet, sandboxesPath, answer(500, `oops`))
	p := newTestProvider(t, api)

	_, err := p.Create(context.Background(), agentSpec())
	if err == nil || !strings.Contains(err.Error(), "could not be confirmed in the list") || !strings.Contains(err.Error(), "it was destroyed") {
		t.Fatalf("err = %v, want one saying the sandbox could not be confirmed and was destroyed", err)
	}
	if got := len(api.sent(http.MethodGet, sandboxesPath)); got < 3 {
		t.Errorf("lists = %d, want the list read again for the whole window", got)
	}
	if got := len(api.sent(http.MethodPost, envdStart)) + len(api.sent(http.MethodPost, envdFiles)); got != 0 {
		t.Errorf("%d calls were made into a sandbox that was never confirmed", got)
	}
}

func TestCreateGivesUpOnASupervisorThatNeverWritesItsState(t *testing.T) {
	api := newFakeAPI(t)
	creatable(api, "hl-agent-pe1-a2")
	api.on(http.MethodGet, envdFiles, answer(404, noFileJSON))
	p := newTestProvider(t, api)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
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

func TestCreateSaysWhenTheSandboxItCouldNotFinishMayStillBeRented(t *testing.T) {
	api := newFakeAPI(t)
	creatable(api, "hl-agent-pe1-a2")
	api.on(http.MethodPost, envdFiles, answer(500, `oops`))
	api.on(http.MethodDelete, sandboxPath, answer(500, `oops`))
	p := newTestProvider(t, api)

	_, err := p.Create(context.Background(), agentSpec())
	if err == nil || !strings.Contains(err.Error(), "may still be rented") || !strings.Contains(err.Error(), "sb1") {
		t.Fatalf("err = %v, want one naming the sandbox that may still be rented", err)
	}
}
