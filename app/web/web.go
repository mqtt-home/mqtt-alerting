package web

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/mqtt-home/mqtt-mail/config"
	"github.com/mqtt-home/mqtt-mail/mail"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/philipparndt/go-logger"
	loggerchi "github.com/philipparndt/go-logger/chi"
)

type SSEClient struct {
	ID      string
	Channel chan string
}

type WebServer struct {
	client *mail.Client
	router *chi.Mux

	sseClients   map[string]*SSEClient
	sseClientsMu sync.RWMutex

	// Liveness: when the bridge first became unhealthy, nil while healthy. The
	// probe only fails once this exceeds the grace window, so transient blips
	// don't cause a restart loop.
	unhealthySince *time.Time
	unhealthyMu    sync.Mutex
}

func NewWebServer(client *mail.Client) *WebServer {
	ws := &WebServer{
		client:     client,
		router:     chi.NewRouter(),
		sseClients: make(map[string]*SSEClient),
	}
	ws.setupRoutes()
	return ws
}

func (ws *WebServer) setupRoutes() {
	ws.router.Use(loggerchi.LoggerWithLevel(slog.LevelDebug))
	ws.router.Use(middleware.Recoverer)

	ws.router.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	ws.router.Route("/api", func(r chi.Router) {
		r.Get("/health", ws.healthCheck)
		r.Get("/livez", ws.liveness)
		r.Get("/status", ws.getStatus)
		r.Get("/events", ws.handleSSE)
		// TODO: command endpoints
	})

	// SPA fallback: serve static files, fall back to index.html for client routes.
	distDir := "./web/dist/"
	fileServer := http.FileServer(http.Dir(distDir))
	ws.router.HandleFunc("/*", func(w http.ResponseWriter, r *http.Request) {
		path := "." + r.URL.Path
		if _, err := http.Dir(distDir).Open(path); err == nil {
			fileServer.ServeHTTP(w, r)
			return
		}
		http.ServeFile(w, r, distDir+"index.html")
	})
}

func (ws *WebServer) getStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ws.client.GetStatus())
}

// healthCheck is the always-200 diagnostic endpoint. Use /livez for the probe.
func (ws *WebServer) healthCheck(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":     "ok",
		"goroutines": runtime.NumGoroutine(),
		"connected":  ws.client.IsConnected(),
		"timestamp":  time.Now().UTC().Format(time.RFC3339),
	})
}

// livenessGrace is how long the bridge may stay unhealthy before /livez fails.
func (ws *WebServer) livenessGrace() time.Duration {
	if s := config.Get().Web.LivenessGraceSeconds; s > 0 {
		return time.Duration(s) * time.Second
	}
	return 4 * time.Minute
}

// liveness is the k8s liveness probe. It returns 503 only once the bridge has
// been unrecoverably stuck for longer than the grace window — a pod restart is
// the proven recovery for an expired cloud session or a lost subscription.
func (ws *WebServer) liveness(w http.ResponseWriter, _ *http.Request) {
	healthy := ws.client.IsConnected()
	now := time.Now()
	grace := ws.livenessGrace()

	ws.unhealthyMu.Lock()
	statusCode, newSince, stuckFor := evaluateLiveness(healthy, ws.unhealthySince, now, grace)
	ws.unhealthySince = newSince
	ws.unhealthyMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]any{
		"healthy":         healthy,
		"stuckForSeconds": int(stuckFor.Seconds()),
		"graceSeconds":    int(grace.Seconds()),
		"timestamp":       now.UTC().Format(time.RFC3339),
	})
}

// evaluateLiveness is the pure liveness decision, split out so it is testable
// without a clock or a server. See liveness_test.go.
func evaluateLiveness(healthy bool, unhealthySince *time.Time, now time.Time, grace time.Duration) (statusCode int, newSince *time.Time, stuckFor time.Duration) {
	if healthy {
		return http.StatusOK, nil, 0
	}
	if unhealthySince == nil {
		t := now
		unhealthySince = &t
	}
	stuckFor = now.Sub(*unhealthySince)
	statusCode = http.StatusOK
	if stuckFor > grace {
		statusCode = http.StatusServiceUnavailable
	}
	return statusCode, unhealthySince, stuckFor
}

// --- SSE ---

// BroadcastStatus pushes a status update to every connected web client.
// Sends are non-blocking: a slow client drops frames rather than stalling MQTT.
func (ws *WebServer) BroadcastStatus(status mail.Status) {
	message, err := json.Marshal(status)
	if err != nil {
		return
	}
	messageStr := string(message)

	ws.sseClientsMu.RLock()
	for _, client := range ws.sseClients {
		select {
		case client.Channel <- messageStr:
		default:
		}
	}
	ws.sseClientsMu.RUnlock()
}

func (ws *WebServer) handleSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	clientID := fmt.Sprintf("%d", time.Now().UnixNano())
	channel := make(chan string, 10)

	ws.sseClientsMu.Lock()
	ws.sseClients[clientID] = &SSEClient{ID: clientID, Channel: channel}
	ws.sseClientsMu.Unlock()

	// Initial state, so the UI is correct on first paint.
	if initial, err := json.Marshal(ws.client.GetStatus()); err == nil {
		fmt.Fprintf(w, "data: %s\n\n", string(initial))
	}

	flusher, ok := w.(http.Flusher)
	if ok {
		flusher.Flush()
	}

	defer func() {
		ws.sseClientsMu.Lock()
		delete(ws.sseClients, clientID)
		close(channel)
		ws.sseClientsMu.Unlock()
	}()

	for {
		select {
		case msg := <-channel:
			if _, err := fmt.Fprintf(w, "data: %s\n\n", msg); err != nil {
				return
			}
			if ok {
				flusher.Flush()
			}
		case <-r.Context().Done():
			return
		}
	}
}

func (ws *WebServer) Start(port int) error {
	addr := ":" + strconv.Itoa(port)
	logger.Info("Starting web server", "address", addr)
	return http.ListenAndServe(addr, ws.router)
}
