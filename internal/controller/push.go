package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/AaronRaj06/portcullis/internal/config"
)

// Pusher sends config snapshots to data-plane admin APIs (PUT /config).
type Pusher struct {
	Client *http.Client
	Token  string // bearer token shared with the data planes
	TLS    bool   // the admin APIs are HTTPS, and Client verifies them
}

// Snapshot is a validated config, its canonical JSON and its hash, the value
// every data plane must acknowledge.
type Snapshot struct {
	Body []byte
	Hash string
}

// NewSnapshot validates c (applying defaults) and versions it by its own hash,
// so pushing the same desired state twice is a no-op on the data plane.
func NewSnapshot(c *config.Config) (*Snapshot, error) {
	c.Version = ""
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("translated config is invalid (controller bug): %w", err)
	}
	c.Version = "sha256:" + c.Hash()[:16]
	body, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	return &Snapshot{Body: body, Hash: c.Hash()}, nil
}

// Push sends s to every endpoint ("ip:port") in parallel and returns an error
// naming each one that failed or acknowledged a different hash. The caller
// retries with back-off (the controller-runtime work queue does).
func (p *Pusher) Push(ctx context.Context, endpoints []string, s *Snapshot) error {
	if len(endpoints) == 0 {
		return ErrNoDataPlane
	}
	errs := make([]error, len(endpoints))
	var wg sync.WaitGroup
	for i, ep := range endpoints {
		wg.Go(func() { errs[i] = p.pushOne(ctx, ep, s) })
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (p *Pusher) pushOne(ctx context.Context, ep string, s *Snapshot) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	scheme := "http://"
	if p.TLS {
		scheme = "https://"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, scheme+ep+"/config", bytes.NewReader(s.Body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", ep, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s: %s", ep, resp.Status, bytes.TrimSpace(body))
	}
	var st config.Status
	if err := json.Unmarshal(body, &st); err != nil {
		return fmt.Errorf("%s: bad acknowledgement: %w", ep, err)
	}
	if st.Hash != s.Hash { // the ack must be for exactly what we sent
		return fmt.Errorf("%s: acknowledged hash %.12s, sent %.12s", ep, st.Hash, s.Hash)
	}
	return nil
}
