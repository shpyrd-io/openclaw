package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeChildren records what the program asked for.
type fakeChildren struct {
	mu       sync.Mutex
	ensured  []string
	restarts []string
}

func (f *fakeChildren) Ensure(name string, argv []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensured = append(f.ensured, name+" "+strings.Join(argv, " "))
}
func (f *fakeChildren) Restart(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarts = append(f.restarts, name)
}
func (f *fakeChildren) Stop() {}

// fakeCLI is an openclaw command that records its calls in calls.log, in
// the state directory it runs in, and refuses a model named boom.
func fakeCLI(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "openclaw")
	script := `#!/bin/sh
echo "$*" >> "$PWD/calls.log"
case "$*" in
  *boom*) echo "no such model"; exit 2 ;;
esac
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// modelsServer answers /models like OpenRouter: a list for the right key,
// 401 otherwise.
func modelsServer(t *testing.T, key string, ids ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+key && r.Header.Get("x-api-key") != key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var data []map[string]string
		for _, id := range ids {
			data = append(data, map[string]string{"id": id})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
}

type rig struct {
	t        *testing.T
	p        *Program
	srv      *httptest.Server
	home     string
	children *fakeChildren
	dash     *httptest.Server
	client   *http.Client
}

func newRig(t *testing.T, opts Options) *rig {
	t.Helper()
	home := t.TempDir()
	bin := t.TempDir()
	opts.Home = home
	opts.Command = []string{fakeCLI(t, bin)}
	ch := &fakeChildren{}
	opts.Children = ch
	dash := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "gateway "+r.URL.Path+" via "+r.Header.Get("X-Forwarded-Host")+" origin "+r.Header.Get("Origin")+" user "+r.Header.Get("X-Forwarded-User"))
	}))
	t.Cleanup(dash.Close)
	opts.Gateway = dash.URL
	p := New(opts)
	close(p.prepared) // the rigs that call Start reopen it
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)
	client := srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &rig{t: t, p: p, srv: srv, home: home, children: ch, dash: dash, client: client}
}

// get asks as a browser would, for a page.
func (r *rig) get(path string) (int, string, http.Header) {
	r.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, r.srv.URL+path, nil)
	req.Header.Set("Accept", "text/html,*/*")
	res, err := r.client.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b), res.Header
}

// post sends JSON to the setup's API and decodes the answer.
func (r *rig) post(path string, body any) (int, map[string]any) {
	r.t.Helper()
	b, _ := json.Marshal(body)
	res, err := r.client.Post(r.srv.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		r.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func (r *rig) state() map[string]any {
	r.t.Helper()
	res, err := r.client.Get(r.srv.URL + "/setup/api/state")
	if err != nil {
		r.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return out
}

func (r *rig) calls() string {
	b, _ := os.ReadFile(filepath.Join(r.home, "calls.log"))
	return string(b)
}

func (r *rig) setUp() {
	r.t.Helper()
	_ = os.MkdirAll(filepath.Join(r.home, ".shpyrd"), 0o755)
	_ = os.WriteFile(filepath.Join(r.home, setupFile), []byte(`{"provider":"openrouter","model":"m"}`), 0o644)
}

func errorOf(out map[string]any) string {
	s, _ := out["error"].(string)
	return s
}

func models(out map[string]any) []string {
	var ids []string
	for _, m := range out["models"].([]any) {
		ids = append(ids, m.(string))
	}
	return ids
}

func TestBeforeSetupEveryPageIsTheSetup(t *testing.T) {
	r := newRig(t, Options{})
	for _, path := range []string{"/", "/sessions", "/api/anything"} {
		status, _, hdr := r.get(path)
		if status != http.StatusSeeOther || hdr.Get("Location") != "/setup" {
			t.Errorf("%s: %d %q, want a redirect to the setup", path, status, hdr.Get("Location"))
		}
	}
	// A program asking for JSON is told in words, not redirected.
	res, _ := r.client.Get(r.srv.URL + "/api/status")
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("a program's request: %d", res.StatusCode)
	}
	// The platform's health check, a plain GET of /, finds the door up.
	res, _ = r.client.Get(r.srv.URL + "/")
	if res.StatusCode != http.StatusOK {
		t.Errorf("the health check: %d", res.StatusCode)
	}
	st := r.state()
	if st["configured"] != false || len(st["providers"].([]any)) != 3 {
		t.Fatalf("state: %v", st)
	}
	if len(r.children.ensured) != 0 {
		t.Errorf("nothing should run before setup, got %v", r.children.ensured)
	}
}

func TestTheSetupPageAndItsFilesAreServed(t *testing.T) {
	r := newRig(t, Options{})
	status, body, hdr := r.get("/setup")
	if status != 200 || !strings.Contains(body, "<html") || !strings.Contains(hdr.Get("Content-Type"), "text/html") {
		t.Fatalf("the page: %d %q %q", status, hdr.Get("Content-Type"), body[:min(len(body), 80)])
	}
	status2, body2, _ := r.get("/setup/anything/else")
	if status2 != 200 || body2 != body {
		t.Fatalf("one document: %d", status2)
	}
	status, body, hdr = r.get("/setup/theme.js")
	if status != 200 || !strings.Contains(body, "shpyrd.theme") || !strings.Contains(hdr.Get("Content-Type"), "javascript") {
		t.Fatalf("theme.js: %d %q", status, hdr.Get("Content-Type"))
	}
}

func TestARefusedKeyIsSaidInWords(t *testing.T) {
	ms := modelsServer(t, "good", "a/one")
	defer ms.Close()
	r := newRig(t, Options{Bases: map[string]string{"openrouter": ms.URL}})
	status, out := r.post("/setup/api/check", map[string]string{"provider": "openrouter", "key": "wrong-key-xyz"})
	if status != http.StatusUnprocessableEntity || !strings.Contains(errorOf(out), "OpenRouter refused the key") {
		t.Fatalf("%d %v", status, out)
	}
	if strings.Contains(errorOf(out), "wrong-key-xyz") {
		t.Error("the key must not be echoed")
	}
	status, out = r.post("/setup/api/check", map[string]string{"provider": "openrouter", "key": ""})
	if status != http.StatusUnprocessableEntity || !strings.Contains(errorOf(out), "The key is needed") {
		t.Fatalf("%d %v", status, out)
	}
}

func TestTheWholeSetup(t *testing.T) {
	ms := modelsServer(t, "sk-or-1", "zeta/z", "anthropic/claude", "minimax/m3")
	defer ms.Close()
	r := newRig(t, Options{Bases: map[string]string{"openrouter": ms.URL}})

	status, out := r.post("/setup/api/check", map[string]string{"provider": "openrouter", "key": "sk-or-1"})
	if status != 200 || out["name"] != "OpenRouter" {
		t.Fatalf("check: %d %v", status, out)
	}
	if got := strings.Join(models(out), ","); got != "anthropic/claude,minimax/m3,zeta/z" {
		t.Fatalf("models should be listed sorted: %s", got)
	}
	tok := out["token"].(string)
	if b, _ := json.Marshal(out); strings.Contains(string(b), "sk-or-1") {
		t.Fatal("the key went back to the browser")
	}

	status, out = r.post("/setup/api/apply", map[string]string{"token": tok, "model": "not/listed"})
	if status != http.StatusUnprocessableEntity || !strings.Contains(errorOf(out), "Choose one of the models listed") {
		t.Fatalf("unknown model: %d %v", status, out)
	}

	status, out = r.post("/setup/api/apply", map[string]string{"token": tok, "model": "minimax/m3"})
	if status != 200 || out["configured"] != true || out["model"] != "minimax/m3" {
		t.Fatalf("apply: %d %v", status, out)
	}
	want := "config set env.vars.OPENROUTER_API_KEY sk-or-1\nmodels set openrouter/minimax/m3\n"
	if got := r.calls(); got != want {
		t.Fatalf("openclaw calls:\n%s\nwant:\n%s", got, want)
	}
	if got := strings.Join(r.children.ensured, "; "); got != "gateway gateway" {
		t.Fatalf("children: %s", got)
	}

	// Now the door is the Control UI, and the setup still opens.
	status, body, _ := r.get("/sessions")
	if status != 200 || !strings.Contains(body, "gateway /sessions via 127.0.0.1") {
		t.Fatalf("door: %d %q", status, body)
	}
	if st := r.state(); st["configured"] != true || st["dashboardReady"] != true {
		t.Fatalf("state after: %v", st)
	}
	// A second check with the same provider says which model is current.
	_, out = r.post("/setup/api/check", map[string]string{"provider": "openrouter", "key": "sk-or-1"})
	if out["chosen"] != "minimax/m3" {
		t.Fatalf("current model not told: %v", out)
	}
}

func TestAKeyGivenAtInstallNeedsNoTyping(t *testing.T) {
	ms := modelsServer(t, "sk-install", "a/one")
	defer ms.Close()
	r := newRig(t, Options{Bases: map[string]string{"openrouter": ms.URL}, InstallKey: "sk-install"})
	if st := r.state(); st["installKey"] != true {
		t.Fatalf("no word of the install key: %v", st)
	}
	status, out := r.post("/setup/api/check", map[string]string{"provider": "openrouter", "key": ""})
	if status != 200 {
		t.Fatalf("check with the install key: %d %v", status, out)
	}
	r.post("/setup/api/apply", map[string]string{"token": out["token"].(string), "model": "a/one"})
	if got := r.calls(); strings.Contains(got, "OPENROUTER_API_KEY") || !strings.Contains(got, "models set openrouter/a/one") {
		t.Fatalf("the install key is in the environment already; openclaw calls:\n%s", got)
	}
}

func TestACustomServerTakesItsAddress(t *testing.T) {
	ms := modelsServer(t, "", "llama")
	defer ms.Close()
	r := newRig(t, Options{})
	status, out := r.post("/setup/api/check", map[string]string{"provider": "custom", "base": "not a url"})
	if status != http.StatusUnprocessableEntity || !strings.Contains(errorOf(out), "full URL") {
		t.Fatalf("%d %v", status, out)
	}
	status, out = r.post("/setup/api/check", map[string]string{"provider": "custom", "base": ms.URL + "/"})
	if status != 200 {
		t.Fatalf("%d %v", status, out)
	}
	r.post("/setup/api/apply", map[string]string{"token": out["token"].(string), "model": "llama"})
	got := r.calls()
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "config set models.providers.custom {") || !strings.Contains(lines[0], `"baseUrl":"`+ms.URL+`"`) || !strings.Contains(lines[0], `"api":"openai-completions"`) || strings.Contains(lines[0], "apiKey") || !strings.HasSuffix(lines[0], "--strict-json --replace") || lines[1] != "models set custom/llama" {
		t.Fatalf("openclaw calls:\n%s", got)
	}
}

func TestAnthropicIsAskedWithItsOwnHeaders(t *testing.T) {
	ms := modelsServer(t, "sk-ant", "claude-x")
	defer ms.Close()
	r := newRig(t, Options{Bases: map[string]string{"anthropic": ms.URL}})
	status, out := r.post("/setup/api/check", map[string]string{"provider": "anthropic", "key": "sk-ant"})
	if status != 200 || strings.Join(models(out), ",") != "claude-x" {
		t.Fatalf("%d %v", status, out)
	}
}

func TestAFailedCommandIsSaid(t *testing.T) {
	ms := modelsServer(t, "k", "boom")
	defer ms.Close()
	r := newRig(t, Options{Bases: map[string]string{"openrouter": ms.URL}})
	_, out := r.post("/setup/api/check", map[string]string{"provider": "openrouter", "key": "k"})
	status, out := r.post("/setup/api/apply", map[string]string{"token": out["token"].(string), "model": "boom"})
	if status != http.StatusInternalServerError || !strings.Contains(errorOf(out), "openclaw models set openrouter/boom failed") || !strings.Contains(errorOf(out), "no such model") {
		t.Fatalf("%d %v", status, out)
	}
	if _, ok := r.p.current(); ok {
		t.Fatal("a failed setup must not be recorded")
	}
}

func TestAnExpiredTokenAsksAgain(t *testing.T) {
	r := newRig(t, Options{})
	status, out := r.post("/setup/api/apply", map[string]string{"token": "nope", "model": "x"})
	if status != http.StatusUnprocessableEntity || !strings.Contains(errorOf(out), "expired") {
		t.Fatalf("%d %v", status, out)
	}
}

func TestASecondSetupRestartsTheGateway(t *testing.T) {
	ms := modelsServer(t, "k", "a/one", "a/two")
	defer ms.Close()
	r := newRig(t, Options{Bases: map[string]string{"openrouter": ms.URL}})
	finish := func(model string) {
		t.Helper()
		_, out := r.post("/setup/api/check", map[string]string{"provider": "openrouter", "key": "k"})
		if status, out := r.post("/setup/api/apply", map[string]string{"token": out["token"].(string), "model": model}); status != 200 {
			t.Fatalf("apply: %d %v", status, out)
		}
	}
	finish("a/one")
	if len(r.children.restarts) != 0 {
		t.Fatalf("the first setup starts the gateway, it does not restart it: %v", r.children.restarts)
	}
	finish("a/two")
	if got := strings.Join(r.children.restarts, ","); got != "gateway" {
		t.Fatalf("restarts after the second setup: %q", got)
	}
}

func TestTheGatewayIsToldWhoThePersonIs(t *testing.T) {
	r := newRig(t, Options{})
	r.setUp()
	req, _ := http.NewRequest(http.MethodGet, r.srv.URL+"/", nil)
	req.Header.Set("X-Shpyrd-User", "ana@example.test")
	req.Header.Set("X-Forwarded-User", "mallory") // a client's own claim is replaced
	res, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(b), "user ana@example.test") {
		t.Fatalf("the gateway saw: %q", b)
	}
	_, body, _ := r.get("/")
	if !strings.Contains(body, "user visitor") {
		t.Fatalf("nobody signed in: %q", body)
	}
}

func TestTheGatewaySeesTheProxysOriginAndOtherSitesAreRefused(t *testing.T) {
	r := newRig(t, Options{})
	r.setUp()
	own := strings.TrimPrefix(r.srv.URL, "http://")
	for _, c := range []struct {
		origin string
		status int
		want   string
	}{
		{"", 200, "origin  user"},
		{"http://" + own, 200, "origin " + r.dash.URL},
		{"http://" + strings.ToUpper(own), 200, "origin " + r.dash.URL},
		{"null", 200, "origin " + r.dash.URL},
		{"https://evil.example", 403, "another site"},
		{"garbage", 403, "another site"},
	} {
		req, _ := http.NewRequest(http.MethodPost, r.srv.URL+"/api/ws", nil)
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		res, err := r.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != c.status || !strings.Contains(string(b)+"\n", c.want) {
			t.Errorf("origin %q: %d %q, want %d containing %q", c.origin, res.StatusCode, b, c.status, c.want)
		}
	}
}

func TestTheDoorSendsToTheSetupWhileTheGatewayIsDown(t *testing.T) {
	r := newRig(t, Options{})
	r.setUp()
	r.dash.Close() // nobody on the gateway's port
	status, _, hdr := r.get("/")
	if status != http.StatusSeeOther || hdr.Get("Location") != "/setup?starting" {
		t.Fatalf("%d %q", status, hdr.Get("Location"))
	}
	if st := r.state(); st["configured"] != true || st["dashboardReady"] != false {
		t.Fatalf("state: %v", st)
	}
}

func TestStartRunsTheChildrenWhenSetUpBefore(t *testing.T) {
	r := newRig(t, Options{})
	r.setUp()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.p.prepared = make(chan struct{})
	r.p.Start(ctx)
	<-r.p.prepared
	r.children.mu.Lock()
	n := len(r.children.ensured)
	r.children.mu.Unlock()
	if n != 1 {
		t.Fatalf("children: %v", r.children.ensured)
	}
	got := r.calls()
	if !strings.HasPrefix(got, "config set --batch-json [") || !strings.Contains(got, `"gateway.auth.mode","value":"trusted-proxy"`) {
		t.Fatalf("the standing configuration was not written at start:\n%s", got)
	}
}
