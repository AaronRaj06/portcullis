// Package admin serves the data plane's admin API on its own port:
//
//	GET  /healthz       liveness: the process is up
//	GET  /readyz        readiness: a config is serving and we are not draining
//	GET  /metrics       Prometheus
//	GET  /config        current version, hash and generation (?full=1 adds the config)
//	PUT  /config        push a new config (JSON or YAML); validated before it is swapped in
//	POST /reload        re-read the config file
//	     /debug/pprof/  profiling, only if enabled
//
// If a token is configured, everything except /healthz, /readyz and /metrics
// requires "Authorization: Bearer <token>".
package admin

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/pprof"
	"strings"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/AaronRaj06/portcullis/internal/config"
)

// Server is the admin API.
type Server struct {
	Manager  *config.Manager
	Gatherer prometheus.Gatherer
	Token    string // empty: no authentication (bind the admin port to localhost)
	Pprof    bool

	draining atomic.Bool
}

// SetDraining makes /readyz fail, so load balancers and Kubernetes stop
// sending new traffic while we drain.
func (s *Server) SetDraining() { s.draining.Store(true) }

// Handler returns the admin mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.Gatherer, promhttp.HandlerOpts{}))
	mux.Handle("GET /config", s.auth(http.HandlerFunc(s.getConfig)))
	mux.Handle("PUT /config", s.auth(http.HandlerFunc(s.putConfig)))
	mux.Handle("POST /reload", s.auth(http.HandlerFunc(s.reload)))
	if s.Pprof {
		mux.Handle("/debug/pprof/", s.auth(http.HandlerFunc(pprof.Index)))
		mux.Handle("/debug/pprof/cmdline", s.auth(http.HandlerFunc(pprof.Cmdline)))
		mux.Handle("/debug/pprof/profile", s.auth(http.HandlerFunc(pprof.Profile)))
		mux.Handle("/debug/pprof/symbol", s.auth(http.HandlerFunc(pprof.Symbol)))
		mux.Handle("/debug/pprof/trace", s.auth(http.HandlerFunc(pprof.Trace)))
	}
	return mux
}

func (s *Server) auth(next http.Handler) http.Handler {
	if s.Token == "" {
		return next
	}
	want := []byte("Bearer " + s.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Constant-time compare: timing must not reveal how much of a guess was right.
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="portcullis-admin"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or wrong bearer token"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) {
	switch {
	case s.draining.Load():
		http.Error(w, "draining", http.StatusServiceUnavailable)
	case s.Manager.Status().Generation == 0:
		http.Error(w, "no config applied", http.StatusServiceUnavailable)
	default:
		_, _ = io.WriteString(w, "ready\n")
	}
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	st := s.Manager.Status()
	if r.URL.Query().Get("full") == "" {
		writeJSON(w, http.StatusOK, st)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		config.Status
		Config *config.Config `json:"config"`
	}{st, s.Manager.Config()})
}

// putConfig is how the controller pushes snapshots. The response is the
// acknowledgement: 200 with the applied hash and generation, or 400 with every
// validation problem (the old config keeps serving).
func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, config.MaxSize))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": err.Error()})
		return
	}
	st, err := s.Manager.Apply(raw, "api")
	s.reply(w, st, err)
}

func (s *Server) reload(w http.ResponseWriter, _ *http.Request) {
	st, err := s.Manager.Reload()
	s.reply(w, st, err)
}

func (s *Server) reply(w http.ResponseWriter, st config.Status, err error) {
	if err == nil {
		writeJSON(w, http.StatusOK, st)
		return
	}
	var ve *config.ValidationError
	problems := []string{err.Error()}
	if errors.As(err, &ve) {
		problems = ve.Problems
	}
	writeJSON(w, http.StatusBadRequest, struct {
		Error    string        `json:"error"`
		Problems []string      `json:"problems"`
		Current  config.Status `json:"current"` // what is still serving
	}{"config rejected", problems, st})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// TokenFromFile reads a bearer token, trimming whitespace. Reading it from a
// file (or a Kubernetes Secret mounted as one) keeps it out of `ps` output.
func TokenFromFile(data []byte) (string, error) {
	t := strings.TrimSpace(string(data))
	if t == "" {
		return "", errors.New("admin token file is empty")
	}
	return t, nil
}
