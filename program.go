package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Options of a Program; the zero values are the container's.
type Options struct {
	Home       string            // OpenClaw's state directory, inside the volume
	Command    []string          // the OpenClaw CLI: the program and its leading arguments
	Gateway    string            // the gateway's address on loopback
	GatewayArg []string          // how the gateway is started
	Children   children          // runs the gateway
	Client     *http.Client      // reaches the providers
	Bases      map[string]string // a provider's base URL, for tests
	InstallKey string            // an OpenRouter key given at install, if any
	Now        func() time.Time
}

// Program is the setup screen and, once set up, the door to the Control UI.
type Program struct {
	o     Options
	proxy *httputil.ReverseProxy
	gw    *url.URL
	web   http.Handler

	mu      sync.Mutex
	pending map[string]*pending
	running bool // the gateway was asked for

	prepared chan struct{} // closed once the standing configuration is written
}

// pending is a checked key waiting for the model choice; the key never
// goes back to the browser.
type pending struct {
	Provider provider
	Base     string
	Key      string
	FromEnv  bool
	Models   []string
	Made     time.Time
}

// setup is what the person chose, kept beside OpenClaw's files.
type setup struct {
	Provider string    `json:"provider"`
	Model    string    `json:"model"`
	Base     string    `json:"base,omitempty"`
	DoneAt   time.Time `json:"doneAt"`
}

const setupFile = ".shpyrd/setup.json"

// The gateway port inside the container; the Control UI is served there.
const gatewayPort = "18789"

// The header the platform's edge sets with who is signed in, and the one
// OpenClaw's trusted-proxy auth reads it from.
const (
	identityHeader = "X-Shpyrd-User"
	userHeader     = "X-Forwarded-User"
)

// baseConfig is the gateway's standing configuration: local, on loopback,
// trusting this proxy for the person's identity, browsers approved as
// they come since the platform's sign-in stands in front. Applied before
// every start; the person's provider and model come on top.
var baseConfig = []map[string]any{
	{"path": "gateway.mode", "value": "local"},
	{"path": "gateway.bind", "value": "loopback"},
	{"path": "gateway.port", "value": 18789},
	{"path": "gateway.auth.mode", "value": "trusted-proxy"},
	{"path": "gateway.auth.trustedProxy.userHeader", "value": strings.ToLower(userHeader)},
	{"path": "gateway.auth.trustedProxy.allowLoopback", "value": true},
	{"path": "gateway.auth.trustedProxy.deviceAutoApprove.enabled", "value": true},
	{"path": "gateway.auth.trustedProxy.deviceAutoApprove.scopes", "value": []string{"operator.admin", "operator.read", "operator.write", "operator.approvals", "operator.questions", "operator.pairing"}},
	{"path": "gateway.trustedProxies", "value": []string{"127.0.0.1"}},
	{"path": "gateway.controlUi.allowedOrigins", "value": []string{"http://127.0.0.1:" + gatewayPort}},
}

func New(o Options) *Program {
	if len(o.Command) == 0 {
		o.Command = []string{"node", "/app/openclaw.mjs"}
	}
	if o.Gateway == "" {
		o.Gateway = "http://127.0.0.1:" + gatewayPort
	}
	if len(o.GatewayArg) == 0 {
		o.GatewayArg = []string{"gateway"}
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 20 * time.Second}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	gw, err := url.Parse(o.Gateway)
	if err != nil {
		panic(err)
	}
	p := &Program{o: o, gw: gw, pending: map[string]*pending{}, web: webHandler(), prepared: make(chan struct{})}
	p.proxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(gw)
			r.SetXForwarded()
			// Who the person is: the edge's header, handed to the gateway
			// under the name its trusted-proxy auth reads. Anything a client
			// sent under that name is replaced.
			who := strings.TrimSpace(r.In.Header.Get(identityHeader))
			if who == "" {
				who = "visitor" // a public project: the edge signs nobody in
			}
			r.Out.Header.Set(userHeader, who)
			// The gateway, bound to loopback, trusts only its own origin: a
			// browser's Origin at the project's address would be refused.
			// The door checked that the Origin is the project's own (see
			// crossSite), so the gateway is told the proxy's.
			if r.In.Header.Get("Origin") != "" {
				r.Out.Header.Set("Origin", gw.Scheme+"://"+gw.Host)
			}
		},
		ErrorHandler: p.notUp,
	}
	return p
}

// crossSite says whether a request with an Origin header comes from a page
// that is not the project's own: the check the gateway would make, kept
// here because the proxy rewrites the Origin.
func crossSite(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return true
	}
	return !strings.EqualFold(u.Host, r.Host)
}

// Handler serves the setup and the Control UI.
func (p *Program) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+webPrefix+"/api/state", p.state)
	mux.HandleFunc("POST "+webPrefix+"/api/check", p.check)
	mux.HandleFunc("POST "+webPrefix+"/api/apply", p.apply)
	mux.Handle(webPrefix, p.web)
	mux.Handle(webPrefix+"/", p.web)
	mux.HandleFunc("/", p.door)
	return mux
}

// Start writes the standing configuration and runs the gateway when the
// setup was done before: in the background, so the page answers at once;
// the doctor's first run takes a while.
func (p *Program) Start(ctx context.Context) {
	go func() {
		defer close(p.prepared)
		if err := p.prepareConfig(ctx); err != nil {
			log.Printf("openclaw-setup: %v", err)
		}
		if _, ok := p.current(); ok {
			p.ensureChildren()
		}
	}()
}

// awaitPrepared waits for the standing configuration, which the setup's
// commands must not race.
func (p *Program) awaitPrepared(ctx context.Context) bool {
	select {
	case <-p.prepared:
		return true
	case <-ctx.Done():
		return false
	}
}

// prepareConfig writes the standing configuration. (The image's own
// entrypoint also runs `openclaw doctor --fix` here; on a fresh volume it
// was seen hanging for minutes, so the gateway is left to repair its
// state itself.)
func (p *Program) prepareConfig(ctx context.Context) error {
	batch, _ := json.Marshal(baseConfig)
	return p.cli(ctx, "config", "set", "--batch-json", string(batch))
}

func (p *Program) current() (setup, bool) {
	b, err := os.ReadFile(filepath.Join(p.o.Home, setupFile))
	if err != nil {
		return setup{}, false
	}
	var s setup
	if json.Unmarshal(b, &s) != nil || s.Model == "" {
		return setup{}, false
	}
	return s, true
}

func (p *Program) ensureChildren() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running || p.o.Children == nil {
		return
	}
	p.running = true
	p.o.Children.Ensure("gateway", p.o.GatewayArg)
}

// gatewayUp says whether the gateway answers as ready.
func (p *Program) gatewayUp() bool {
	c := &http.Client{Timeout: 500 * time.Millisecond}
	res, err := c.Get(p.o.Gateway + "/readyz")
	if err != nil {
		return false
	}
	res.Body.Close()
	return res.StatusCode == http.StatusOK
}

// door is every path but the setup's: the Control UI once set up and
// answering, the setup page otherwise.
func (p *Program) door(w http.ResponseWriter, r *http.Request) {
	if _, ok := p.current(); !ok {
		p.toSetup(w, r)
		return
	}
	if crossSite(r) {
		http.Error(w, "a request from another site is refused", http.StatusForbidden)
		return
	}
	p.ensureChildren()
	p.proxy.ServeHTTP(w, r)
}

// toSetup sends a page to the setup, which opens OpenClaw by itself once
// the gateway answers when the setup was done already. The platform's
// health checks, a plain GET of /, are told the door is up: the project
// is healthy while it waits to be set up. Any other program's request
// gets the answer in words.
func (p *Program) toSetup(w http.ResponseWriter, r *http.Request) {
	msg := "OpenClaw is not set up or not answering yet; open " + webPrefix
	if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
		to := webPrefix
		if _, ok := p.current(); ok {
			to += "?starting"
		}
		http.Redirect(w, r, to, http.StatusSeeOther)
		return
	}
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.URL.Path == "/" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(msg + "\n"))
		return
	}
	w.Header().Set("Retry-After", "3")
	http.Error(w, msg, http.StatusServiceUnavailable)
}

// notUp is the gateway not answering: the setup page says so and tries
// again until it does.
func (p *Program) notUp(w http.ResponseWriter, r *http.Request, err error) {
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		log.Printf("openclaw-setup: gateway: %v", err)
	}
	p.toSetup(w, r)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v)
}

type providerView struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	KeysAt string `json:"keysAt"`
	Custom bool   `json:"custom"`
}

type stateView struct {
	Configured     bool           `json:"configured"`
	Provider       string         `json:"provider,omitempty"`
	Model          string         `json:"model,omitempty"`
	InstallKey     bool           `json:"installKey"`
	Providers      []providerView `json:"providers"`
	DashboardReady bool           `json:"dashboardReady"`
}

func (p *Program) stateView() stateView {
	v := stateView{InstallKey: p.o.InstallKey != ""}
	for _, pr := range providers {
		v.Providers = append(v.Providers, providerView{ID: pr.ID, Name: pr.Name, KeysAt: pr.KeysAt, Custom: pr.ID == "custom"})
	}
	if s, ok := p.current(); ok {
		v.Configured, v.Provider, v.Model = true, s.Provider, s.Model
		v.DashboardReady = p.gatewayUp()
	}
	return v
}

// state says what is set up and whether the gateway answers.
func (p *Program) state(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, p.stateView())
}

// check tests the key against the provider and lists its models.
func (p *Program) check(w http.ResponseWriter, r *http.Request) {
	var in struct{ Provider, Key, Base string }
	if err := readJSON(w, r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "The request could not be read.")
		return
	}
	prov, ok := providerByID(in.Provider)
	if !ok {
		writeError(w, http.StatusBadRequest, "Choose a provider.")
		return
	}
	base := prov.Base
	if b := p.o.Bases[prov.ID]; b != "" {
		base = b
	}
	if prov.ID == "custom" {
		base = strings.TrimRight(strings.TrimSpace(in.Base), "/")
		if u, err := url.Parse(base); err != nil || u.Scheme == "" || u.Host == "" {
			writeError(w, http.StatusUnprocessableEntity, "The server's address needs to be a full URL, like https://llm.example.com/v1.")
			return
		}
	}
	key := strings.TrimSpace(in.Key)
	fromEnv := false
	if key == "" && prov.ID == "openrouter" && p.o.InstallKey != "" {
		key, fromEnv = p.o.InstallKey, true
	}
	if key == "" && prov.ID != "custom" {
		writeError(w, http.StatusUnprocessableEntity, "The key is needed.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	models, err := listModels(ctx, p.o.Client, prov, base, key)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, errRefused) {
			msg = prov.Name + " refused the key. Check it at " + prov.KeysAt + "."
		}
		writeError(w, http.StatusUnprocessableEntity, msg)
		return
	}
	if len(models) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "The key works, but "+prov.Name+" lists no models for it.")
		return
	}
	tok := p.remember(&pending{Provider: prov, Base: base, Key: key, FromEnv: fromEnv, Models: models, Made: p.o.Now()})
	chosen := ""
	if s, ok := p.current(); ok && s.Provider == prov.ID {
		chosen = s.Model
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "name": prov.Name, "models": models, "chosen": chosen})
}

func (p *Program) remember(pd *pending) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, v := range p.pending {
		if p.o.Now().Sub(v.Made) > 20*time.Minute {
			delete(p.pending, k)
		}
	}
	p.pending[tok] = pd
	return tok
}

func (p *Program) take(tok string) *pending {
	p.mu.Lock()
	defer p.mu.Unlock()
	pd := p.pending[tok]
	delete(p.pending, tok)
	return pd
}

func (p *Program) keep(tok string, pd *pending) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending[tok] = pd
}

// modelRef is the model as OpenClaw names it: provider/model.
func modelRef(pd *pending, model string) string {
	return pd.Provider.ID + "/" + model
}

// commands are the OpenClaw CLI calls that make the choice its own: the
// key into the config's environment (the gateway exports it), a custom
// server as a provider of its own, and the model as the default.
func commands(pd *pending, model string) [][]string {
	var out [][]string
	switch {
	case pd.Provider.ID == "custom":
		prov := map[string]any{
			"baseUrl": pd.Base,
			"api":     "openai-completions",
			"models":  []map[string]string{{"id": model, "name": model}},
		}
		if pd.Key != "" {
			prov["apiKey"] = pd.Key
		}
		b, _ := json.Marshal(prov)
		out = append(out, []string{"config", "set", "models.providers.custom", string(b), "--strict-json", "--replace"})
	case pd.Provider.KeyVar != "" && !pd.FromEnv:
		out = append(out, []string{"config", "set", "env.vars." + pd.Provider.KeyVar, pd.Key})
	}
	return append(out, []string{"models", "set", modelRef(pd, model)})
}

// apply writes the choice through OpenClaw's own commands and starts the
// gateway, or restarts it, since it reads the model and the key at start.
func (p *Program) apply(w http.ResponseWriter, r *http.Request) {
	var in struct{ Token, Model string }
	if err := readJSON(w, r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "The request could not be read.")
		return
	}
	pd := p.take(in.Token)
	if pd == nil {
		writeError(w, http.StatusUnprocessableEntity, "That check has expired. Enter the key again.")
		return
	}
	model := strings.TrimSpace(in.Model)
	known := false
	for _, m := range pd.Models {
		if m == model {
			known = true
			break
		}
	}
	if !known {
		p.keep(in.Token, pd) // the choice can be made again
		writeError(w, http.StatusUnprocessableEntity, "Choose one of the models listed.")
		return
	}
	if !p.awaitPrepared(r.Context()) {
		p.keep(in.Token, pd)
		writeError(w, http.StatusServiceUnavailable, "OpenClaw is still preparing; try again in a moment.")
		return
	}
	for _, args := range commands(pd, model) {
		if err := p.cli(r.Context(), args...); err != nil {
			p.keep(in.Token, pd)
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	s := setup{Provider: pd.Provider.ID, Model: model, DoneAt: p.o.Now()}
	if pd.Provider.ID == "custom" {
		s.Base = pd.Base
	}
	path := filepath.Join(p.o.Home, setupFile)
	b, _ := json.MarshalIndent(s, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		err = os.WriteFile(path, b, 0o644)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "OpenClaw is configured, but the setup could not be recorded: "+err.Error())
			return
		}
	}
	log.Printf("openclaw-setup: set up with %s, model %s", pd.Provider.Name, model)
	p.mu.Lock()
	running := p.running
	p.mu.Unlock()
	if running {
		p.o.Children.Restart("gateway")
	}
	p.ensureChildren()
	writeJSON(w, http.StatusOK, p.stateView())
}

// cli runs one OpenClaw command in the state directory, and says what it
// said when it fails. A key in the arguments is not repeated.
func (p *Program) cli(ctx context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	argv := append(append([]string{}, p.o.Command[1:]...), args...)
	cmd := exec.CommandContext(ctx, p.o.Command[0], argv...)
	cmd.Dir = p.o.Home
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		shown := make([]string, 0, len(args))
		for i, a := range args {
			if i > 0 && (strings.Contains(args[i-1], "_KEY") || strings.Contains(a, "apiKey")) {
				a = "…"
			}
			shown = append(shown, a)
		}
		return fmt.Errorf("openclaw %s failed: %v: %s", strings.Join(shown, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
