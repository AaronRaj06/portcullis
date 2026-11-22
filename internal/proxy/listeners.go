package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Port is what one extra listener serves: its own routes and, for HTTPS, its
// own certificates. Nothing is shared between ports, which is what keeps one
// Gateway's routes off another Gateway's address.
type Port struct {
	Router *Router
	Certs  []Cert // none: plain HTTP
}

// Cert is a certificate and the server name it is for.
type Cert struct {
	Host string // exact, "*.example.com", or "" for any name
	Cert *tls.Certificate
}

// certFor picks by SNI: exact, then the longest wildcard, then the catch-all.
func certFor(certs []Cert, serverName string) *tls.Certificate {
	name := normalizeHost(serverName)
	var best *tls.Certificate
	bestLen := -1
	for _, c := range certs {
		switch {
		case c.Host != "" && c.Host == name:
			return c.Cert
		case c.Host == "":
			if bestLen < 0 {
				best, bestLen = c.Cert, 0
			}
		case strings.HasPrefix(c.Host, "*.") && strings.HasSuffix(name, c.Host[1:]) && len(name) >= len(c.Host):
			if len(c.Host) > bestLen {
				best, bestLen = c.Cert, len(c.Host)
			}
		}
	}
	return best
}

// Listeners opens and closes the config's extra ports as snapshots change.
type Listeners struct {
	h     *Handler
	host  string
	l     Limits
	drain time.Duration

	mu     sync.Mutex
	open   map[int]*portServer
	closed bool
	wg     sync.WaitGroup
}

type portServer struct {
	ln  net.Listener
	srv *http.Server
}

// NewListeners binds on host ("" means every address).
func NewListeners(h *Handler, host string, l Limits, drain time.Duration) *Listeners {
	return &Listeners{h: h, host: host, l: l, drain: drain, open: map[int]*portServer{}}
}

// Open binds every port in want that is not open yet. It is called before the
// snapshot swap, so a port that cannot be bound rejects the whole config.
func (ls *Listeners) Open(want map[int]*Port) error {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.closed {
		return http.ErrServerClosed
	}
	for port, p := range want {
		useTLS := len(p.Certs) > 0
		if cur := ls.open[port]; cur != nil {
			if (cur.srv.TLSConfig != nil) == useTLS {
				continue
			}
			// The port changed protocol: close it and bind it again.
			ls.retain(otherPorts(ls.open, port))
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(ls.host, strconv.Itoa(port)))
		if err != nil {
			return fmt.Errorf("listener %d: %w", port, err)
		}
		var cfg *tls.Config
		if useTLS {
			cfg = baseTLS(func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
				if s := ls.h.Current(); s != nil && s.Ports[port] != nil {
					if c := certFor(s.Ports[port].Certs, hello.ServerName); c != nil {
						return c, nil
					}
				}
				return nil, fmt.Errorf("no certificate for server name %q", hello.ServerName)
			})
		}
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ls.h.serve(w, r, port) })
		ps := &portServer{ln: ln, srv: NewServer(h, ls.l, cfg)}
		ls.open[port] = ps
		go func() {
			var err error
			if cfg != nil {
				err = ps.srv.ServeTLS(ln, "", "")
			} else {
				err = ps.srv.Serve(ln)
			}
			ls.mu.Lock()
			defer ls.mu.Unlock()
			if ls.open[port] == ps { // not closed by us: let the next push bind it again
				delete(ls.open, port)
				ls.h.o.Logger.Error("listener stopped", "port", port, "error", err)
			}
		}()
	}
	return nil
}

func otherPorts(open map[int]*portServer, except int) map[int]*Port {
	keep := map[int]*Port{}
	for p := range open {
		if p != except {
			keep[p] = &Port{}
		}
	}
	return keep
}

// Retain closes every port that is not in keep. The socket closes at once;
// requests in flight drain in the background.
func (ls *Listeners) Retain(keep map[int]*Port) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.retain(keep)
}

func (ls *Listeners) retain(keep map[int]*Port) {
	for port, ps := range ls.open {
		if keep[port] != nil {
			continue
		}
		delete(ls.open, port)
		_ = ps.ln.Close()
		ls.wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), ls.drain)
			defer cancel()
			if ps.srv.Shutdown(ctx) != nil {
				_ = ps.srv.Close()
			}
		})
	}
}

// Close drains every port and waits for them. Upgraded connections are drained
// by Handler.Drain.
func (ls *Listeners) Close() {
	ls.mu.Lock()
	ls.closed = true
	ls.retain(nil)
	ls.mu.Unlock()
	ls.wg.Wait()
}
