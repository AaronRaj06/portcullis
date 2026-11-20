package config

import (
	"context"
	"errors"
	"fmt"

	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/AaronRaj06/portcullis/internal/health"
	"github.com/AaronRaj06/portcullis/internal/metrics"
	"github.com/AaronRaj06/portcullis/internal/proxy"
)

// Status describes the config currently serving traffic.
type Status struct {
	Version    string    `json:"version"`
	Hash       string    `json:"hash"`
	Generation uint64    `json:"generation"`
	Source     string    `json:"source"` // "file:<path>", "api", "flags"
	AppliedAt  time.Time `json:"appliedAt"`
	Changed    bool      `json:"changed"` // false if the pushed config equalled the current one
}

// Manager validates, compiles and atomically publishes configs to a Handler,
// and runs the active health checks of whichever snapshot is current. Applies
// are serialised by a mutex; the request path never touches it.
type Manager struct {
	ctx     context.Context
	h       *proxy.Handler
	limits  proxy.Limits
	path    string
	log     *slog.Logger
	metrics *metrics.Metrics
	checker *http.Client // health checks for pools using the default transport

	mu         sync.Mutex
	status     Status
	cfg        *Config
	stopHealth context.CancelFunc
	transports map[string]*http.Transport
}

// PreconditionError means the caller's If-Match did not name the config that
// is serving, e.g. a stale controller pushing over a newer config.
type PreconditionError struct {
	Expected string
	Current  Status
}

func (e *PreconditionError) Error() string {
	return fmt.Sprintf("precondition failed: expected config %s, serving %s", e.Expected, e.Current.Hash)
}

// NewManager returns a Manager publishing to h. path is the file Reload reads
// ("" if configs only arrive through Apply). Health checks stop when ctx ends.
func NewManager(ctx context.Context, h *proxy.Handler, l proxy.Limits, path string, log *slog.Logger, m *metrics.Metrics) *Manager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Manager{
		ctx: ctx, h: h, limits: l, path: path, log: log, metrics: m,
		checker: healthClient(proxy.NewTransport(l)),
	}
}

func healthClient(t http.RoundTripper) *http.Client {
	return &http.Client{
		Transport:     t,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, // 3xx counts as healthy
	}
}

// Reload re-reads the config file (SIGHUP and POST /reload).
func (m *Manager) Reload() (Status, error) {
	if m.path == "" {
		return m.Status(), errors.New("no config file: started without -config")
	}
	raw, err := os.ReadFile(m.path)
	if err != nil {
		m.record("error")
		return m.Status(), err
	}
	return m.Apply(raw, "file:"+m.path)
}

// Apply parses raw (YAML or JSON) and publishes it if valid. On any error the
// current snapshot keeps serving, untouched.
func (m *Manager) Apply(raw []byte, source string) (Status, error) {
	return m.ApplyIfMatch(raw, source, "")
}

// ApplyIfMatch is Apply with optimistic concurrency: if ifMatch is not empty,
// the config is applied only if the serving config's hash equals it ("*"
// matches any applied config). The check and the swap happen under one lock,
// so two writers cannot both pass the check.
func (m *Manager) ApplyIfMatch(raw []byte, source, ifMatch string) (Status, error) {
	c, err := Parse(raw)
	if err != nil {
		m.record("error")
		return m.Status(), err
	}
	return m.applyConfig(c, source, ifMatch)
}

// ApplyConfig validates, compiles and publishes c.
func (m *Manager) ApplyConfig(c *Config, source string) (Status, error) {
	return m.applyConfig(c, source, "")
}

func (m *Manager) applyConfig(c *Config, source, ifMatch string) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	matches := ifMatch == "" || ifMatch == m.status.Hash || (ifMatch == "*" && m.status.Generation > 0)
	if !matches {
		m.record("conflict")
		return m.status, &PreconditionError{Expected: ifMatch, Current: m.status}
	}
	compiled, err := Compile(c, m.limits, m.h.Current(), m.transports)
	if err != nil {
		m.record("error")
		m.log.Warn("config rejected; previous config keeps serving", "source", source, "error", err.Error())
		return m.status, err
	}
	if cur := m.h.Current(); cur != nil && cur.Hash == compiled.Snapshot.Hash {
		m.record("unchanged") // same hash means same transport keys: nothing new was built
		st := m.status
		st.Changed = false
		return st, nil
	}

	compiled.Snapshot.Generation = m.status.Generation + 1
	m.h.Swap(compiled.Snapshot) // new requests use it from here on; in-flight ones finish on the old one

	if m.stopHealth != nil {
		m.stopHealth()
	}
	hctx, cancel := context.WithCancel(m.ctx)
	m.stopHealth = cancel
	for _, ph := range compiled.Health {
		client := m.checker
		if ph.Pool.Transport != nil {
			client = healthClient(ph.Pool.Transport) // check over the same TLS/h2c settings as traffic
		}
		health.Run(hctx, client, ph.Pool, ph.Config, nil, m.log)
	}
	// Transports no longer used by any pool: drop their idle connections now.
	// Busy ones finish their in-flight requests and then time out idle.
	// Transports with unchanged settings were reused and keep their connections.
	for key, t := range m.transports {
		if compiled.Transports[key] != t {
			t.CloseIdleConnections()
		}
	}
	m.transports = compiled.Transports

	m.cfg = c
	m.status = Status{
		Version: c.Version, Hash: compiled.Snapshot.Hash, Generation: compiled.Snapshot.Generation,
		Source: source, AppliedAt: time.Now().UTC(), Changed: true,
	}
	m.record("success")
	if m.metrics != nil {
		m.metrics.ConfigGeneration(m.status.Generation)
	}
	m.log.Info("config applied", "source", source, "version", c.Version,
		"hash", short(m.status.Hash), "generation", m.status.Generation)
	return m.status, nil
}

// Status returns the current config's status.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

// Config returns the current config (with defaults applied). Do not modify it.
func (m *Manager) Config() *Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

func (m *Manager) record(result string) {
	if m.metrics != nil {
		m.metrics.ConfigReload(result)
	}
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
