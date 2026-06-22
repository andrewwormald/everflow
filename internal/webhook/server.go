// Package webhook hosts the HTTP server that ingests provider webhooks and
// dispatches them to the workflow runtime via workflow.Callback.
// See DESIGN.md § "Architecture".
package webhook

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/andrewwormald/everflow/internal/provider"
)

// Server is the HTTP front for webhook ingestion. Mount it onto an
// http.ServeMux via Mount(); the daemon owns the http.Server lifecycle.
//
// Routing: POST /webhook/{providerName}/{runID}
//   - Looks up the registered runID + secret
//   - Verifies HMAC via the provider
//   - Normalises the event
//   - Calls the registered Dispatcher
type Server struct {
	providers  map[string]provider.Provider
	dispatcher Dispatcher
	secrets    *SecretRegistry

	mu sync.Mutex
}

// Dispatcher receives normalised events bound for a specific runID.
// In production this is a thin wrapper that calls workflow.Callback.
type Dispatcher func(ctx context.Context, runID string, event provider.Event) error

// SecretRegistry maps a (provider, runID) to the HMAC secret we registered
// with that provider's webhook. Per-Run secrets so a leaked one only
// affects one Run.
type SecretRegistry struct {
	mu      sync.RWMutex
	secrets map[string]string // key: provider + "/" + runID
}

func NewSecretRegistry() *SecretRegistry {
	return &SecretRegistry{secrets: map[string]string{}}
}

func (r *SecretRegistry) Set(providerName, runID, secret string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.secrets[providerName+"/"+runID] = secret
}

func (r *SecretRegistry) Get(providerName, runID string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.secrets[providerName+"/"+runID]
	return s, ok
}

func (r *SecretRegistry) Forget(providerName, runID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.secrets, providerName+"/"+runID)
}

// NewServer wires a Server. providers maps name → Provider impl.
func NewServer(providers map[string]provider.Provider, dispatcher Dispatcher, secrets *SecretRegistry) *Server {
	return &Server{
		providers:  providers,
		dispatcher: dispatcher,
		secrets:    secrets,
	}
}

// Mount registers the webhook server's routes (/webhook/{provider}/{runID}
// and /health) on the provided mux. The caller owns the http.Server. This
// lets the daemon mount other routes (e.g. /trigger on a separate listener)
// without webhook-coupling its lifecycle.
func (s *Server) Mount(mux *http.ServeMux) {
	mux.HandleFunc("/webhook/", s.handleWebhook)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/webhook/"), "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		http.Error(w, "expected /webhook/{provider}/{runID}", http.StatusBadRequest)
		return
	}
	providerName, runID := parts[0], parts[1]

	p, ok := s.providers[providerName]
	if !ok {
		http.Error(w, fmt.Sprintf("unknown provider %q", providerName), http.StatusNotFound)
		return
	}
	secret, ok := s.secrets.Get(providerName, runID)
	if !ok {
		http.Error(w, "unknown runID", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if !p.VerifySignature(r.Header, body, secret) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	event, err := p.NormaliseEvent(r.Header, body)
	if err != nil {
		if _, ok := err.(provider.ErrIgnore); ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, "normalise: "+err.Error(), http.StatusBadRequest)
		return
	}

	if err := s.dispatcher(r.Context(), runID, event); err != nil {
		http.Error(w, "dispatch: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
