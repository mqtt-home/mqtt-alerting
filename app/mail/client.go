package mail

import (
	"sync"
	"time"

	"github.com/philipparndt/go-logger"
)

// Status is what gets published to `<topic>/status` and pushed over SSE.
// Keep the JSON tags snake_case — the whole house reads these payloads.
type Status struct {
	Online    bool      `json:"online"`
	UpdatedAt time.Time `json:"updated_at"`
	// TODO: device fields
}

type StatusListener func(Status)

type Client struct {
	host     string
	username string
	password string

	mu        sync.RWMutex
	status    Status
	connected bool

	listeners []StatusListener
}

func NewClient(host, username, password string) *Client {
	return &Client{host: host, username: username, password: password}
}

func (c *Client) AddStatusChangeListener(l StatusListener) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listeners = append(c.listeners, l)
}

func (c *Client) GetStatus() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.status
}

// IsConnected reports whether the last exchange with the device succeeded.
// The liveness probe uses this — see web/web.go.
func (c *Client) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected
}

// Connect performs the initial authentication / handshake.
func (c *Client) Connect() error {
	// TODO: authenticate against the device or cloud API
	return c.Poll()
}

// Poll fetches the current state once and notifies listeners if it changed.
func (c *Client) Poll() error {
	// TODO: fetch the real state
	next := Status{Online: true, UpdatedAt: time.Now()}

	c.mu.Lock()
	changed := next != c.status
	c.status = next
	c.connected = true
	listeners := append([]StatusListener(nil), c.listeners...)
	c.mu.Unlock()

	if changed {
		for _, l := range listeners {
			l(next)
		}
	}
	return nil
}

// StartPolling polls until stop is closed. Errors are logged, never fatal —
// a device that is briefly unreachable must not kill the bridge.
func (c *Client) StartPolling(interval time.Duration, stop <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := c.Poll(); err != nil {
				logger.Error("Poll failed", "error", err)
				c.mu.Lock()
				c.connected = false
				c.mu.Unlock()
			}
		case <-stop:
			logger.Info("Polling stopped")
			return
		}
	}
}
