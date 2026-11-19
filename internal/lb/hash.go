package lb

import (
	"net/http"
	"slices"
	"strconv"
)

// DefaultVnodes is the virtual nodes per unit of weight. Measured with 4 backends,
// it keeps every backend's share of the ring within about 5% of fair; 160 gave 16%.
const DefaultVnodes = 640

// KeyFunc extracts the consistent-hashing key from a request. client is the real
// client IP. "" means no key.
type KeyFunc func(r *http.Request, client string) string

// HeaderKey hashes on a request header.
func HeaderKey(name string) KeyFunc {
	return func(r *http.Request, _ string) string { return r.Header.Get(name) }
}

// CookieKey hashes on a cookie value.
func CookieKey(name string) KeyFunc {
	return func(r *http.Request, _ string) string {
		c, err := r.Cookie(name)
		if err != nil {
			return ""
		}
		return c.Value
	}
}

// ClientIPKey hashes on the real client IP: the TCP peer, or behind trusted
// proxies the original client from X-Forwarded-For (resolved by the proxy).
func ClientIPKey(_ *http.Request, client string) string { return client }

// PathKey hashes on the URL path.
func PathKey(r *http.Request, _ string) string { return r.URL.Path }

// ring is consistent hashing with virtual nodes. Each backend owns
// vnodes×Weight points on a 64-bit ring. A key goes to the first point at or after
// its hash. Removing a backend moves only the keys it owned (about 1/N of them);
// a plain hash-mod-N would move nearly all of them.
type ring struct {
	hashes []uint64 // sorted
	owners []*Backend
	key    KeyFunc
}

// NewConsistentHash builds a ring. A request whose key is empty falls back to the client IP.
func NewConsistentHash(bs []*Backend, key KeyFunc, vnodes int) Balancer {
	type point struct {
		h uint64
		b *Backend
	}
	vnodes = max(vnodes, 1)
	var pts []point
	for _, b := range bs {
		for i := 0; i < vnodes*b.Weight; i++ {
			pts = append(pts, point{hash64(b.Addr + "#" + strconv.Itoa(i)), b})
		}
	}
	slices.SortFunc(pts, func(a, b point) int {
		switch {
		case a.h < b.h:
			return -1
		case a.h > b.h:
			return 1
		}
		return 0
	})
	rg := &ring{key: key, hashes: make([]uint64, len(pts)), owners: make([]*Backend, len(pts))}
	for i, p := range pts {
		rg.hashes[i], rg.owners[i] = p.h, p.b
	}
	return rg
}

func (rg *ring) Pick(r *http.Request, client string, tried []*Backend, now int64) *Backend {
	n := len(rg.hashes)
	if n == 0 {
		return nil
	}
	k := rg.key(r, client)
	if k == "" {
		k = client
	}
	i, _ := slices.BinarySearch(rg.hashes, hash64(k))
	// Walk clockwise past unusable owners. A distinct backend appears within a few
	// steps, so this rarely goes far.
	for j := 0; j < n; j++ {
		if b := rg.owners[(i+j)%n]; usable(b, now, tried) {
			return b
		}
	}
	return nil
}

// hash64 is FNV-1a followed by the splitmix64 finalizer. FNV alone clusters
// similar inputs ("a#1", "a#2"); the finalizer spreads them over the ring.
// It is deterministic, so every proxy replica builds the same ring.
func hash64(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	h ^= h >> 30
	h *= 0xbf58476d1ce4e5b9
	h ^= h >> 27
	h *= 0x94d049bb133111eb
	h ^= h >> 31
	return h
}
