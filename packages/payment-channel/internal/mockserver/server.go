package mockserver

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Options configure the mock server. Zero values are sensible defaults.
type Options struct {
	Logger       Logger
	WebhookDelay time.Duration // applied to async scenarios; 0 = sync inline
	GCash        GCashKeys     // optional pre-loaded key material for GCash RSA
}

// GCashKeys holds the mock gateway's RSA keys. If Priv is nil the server
// generates a fresh pair on startup — handy for tests that don't care about
// key reuse across runs.
type GCashKeys struct {
	Priv           *rsa.PrivateKey // mock gateway signing key (webhook signer)
	MerchantPubPEM string          // merchant's public key (for verifying adapter-signed requests); empty = skip verify
}

// Server is the HTTP mux that exposes every channel's endpoints on a single
// port. It keeps an in-memory store so idempotency + status/refund reads work
// exactly like a real sandbox.
type Server struct {
	mux    *http.ServeMux
	store  *store
	disp   *dispatcher
	logger Logger
	opts   Options

	gcashPriv *rsa.PrivateKey

	mu     sync.Mutex
	custom map[string]http.HandlerFunc // channel override, for tests
}

// New returns a ready-to-serve Server. Call Handler() to embed into an
// existing HTTP stack, or ListenAndServe.
func New(opts Options) (*Server, error) {
	if opts.Logger == nil {
		opts.Logger = NewStdLogger()
	}
	s := &Server{
		mux:    http.NewServeMux(),
		store:  newStore(),
		disp:   newDispatcher(opts.Logger, opts.WebhookDelay),
		logger: opts.Logger,
		opts:   opts,
		custom: make(map[string]http.HandlerFunc),
	}
	priv := opts.GCash.Priv
	if priv == nil {
		var err error
		priv, err = rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, err
		}
	}
	s.gcashPriv = priv
	s.register()
	return s, nil
}

// GCashPublicKeyPEM returns the PEM-encoded public key that adapters must
// trust when verifying webhooks from this mock.
func (s *Server) GCashPublicKeyPEM() string {
	pem, _ := marshalPKIXPublicKeyPEM(&s.gcashPriv.PublicKey)
	return pem
}

// Handler returns the mux so callers can wrap it (logging, auth, etc.).
func (s *Server) Handler() http.Handler { return s.mux }

// ListenAndServe is a convenience for standalone runs / docker-compose.
func (s *Server) ListenAndServe(addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	s.logger.Info("mockserver listening", "addr", addr)
	return srv.ListenAndServe()
}

func (s *Server) register() {
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	s.mux.HandleFunc("/__mock/list", s.handleListPayments)
	s.registerGCash()
	s.registerMaya()
	s.registerGrabPay()
	s.registerPayMongo()
	s.registerXendit()
	s.registerDragonpay()
	s.registerSimple() // coinsph / instapay / pesonet / bdo / bpi / metrobank / landbank / shopeepay / billease
}

// handleListPayments dumps the in-memory store — for local debugging only.
func (s *Server) handleListPayments(w http.ResponseWriter, _ *http.Request) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	out := make([]*Payment, 0, len(s.store.byPay))
	for _, p := range s.store.byPay {
		out = append(out, p)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
