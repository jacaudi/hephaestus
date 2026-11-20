package secrets

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// onePasswordSuffixes are the host suffixes the SDK's service-account traffic goes to.
var onePasswordSuffixes = [...]string{".1password.com", ".1password.ca", ".1password.eu"} //nolint:gochecknoglobals // fixed list of 1Password domains

// counterHours is the width of the rolling window, in hour buckets.
const counterHours = 24

// Counter is an http.RoundTripper that counts the requests it carries to 1Password hosts in hour buckets over a rolling
// 24 hours. The window has hour granularity: a request leaves the count when its hour is 24 hours old, not 24 hours to
// the second.
type Counter struct {
	next http.RoundTripper
	now  func() time.Time

	mu      sync.Mutex
	buckets map[int64]int // hours since the Unix epoch -> requests in that hour
}

// NewCounter returns a Counter that passes every request to next and reads the time from now.
func NewCounter(next http.RoundTripper, now func() time.Time) *Counter {
	return &Counter{next: next, now: now, buckets: make(map[int64]int)}
}

// RoundTrip counts r when its host is a 1Password host, then passes it to the next transport.
func (c *Counter) RoundTrip(r *http.Request) (*http.Response, error) {
	if isOnePasswordHost(r.URL.Hostname()) {
		c.mu.Lock()
		hour := c.hour()
		c.buckets[hour]++
		c.prune(hour)
		c.mu.Unlock()
	}

	return c.next.RoundTrip(r)
}

// Last24h returns the number of requests counted in the last 24 hour buckets, the current hour included.
func (c *Counter) Last24h() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	hour := c.hour()
	c.prune(hour)
	total := 0
	for h, n := range c.buckets {
		if h <= hour {
			total += n
		}
	}

	return total
}

// hour is the current hour bucket.
func (c *Counter) hour() int64 { return c.now().Unix() / int64(time.Hour/time.Second) }

// prune drops the buckets that have left the window ending at hour. The caller holds c.mu.
func (c *Counter) prune(hour int64) {
	for h := range c.buckets {
		if h <= hour-counterHours {
			delete(c.buckets, h)
		}
	}
}

// isOnePasswordHost reports whether host is a subdomain of a 1Password domain.
func isOnePasswordHost(host string) bool {
	host = strings.ToLower(host)
	for _, suffix := range onePasswordSuffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}

	return false
}
