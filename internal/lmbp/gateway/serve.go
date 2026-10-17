package gateway

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/store"
)

// ServeOptions configures a standalone gateway process. Secrets come from the
// environment: LSP_SIGNING_SEED (32-byte hex Ed25519 seed; required unless
// AllowEphemeralKey), LSP_DATA_KEY (optional 32-byte hex AES key for data at
// rest) and one env var per tenant API key.
type ServeOptions struct {
	Listen            string
	DB                string
	Tenants           string // "acme:ENV_VAR,beta:ENV_VAR2"
	Retention         time.Duration
	AllowEphemeralKey bool
	// Issuer is the public https origin of this gateway (required unless
	// AllowEphemeralKey, i.e. development, where it is derived per request).
	Issuer string
	// Profiles is an optional JSON file {"tenant": TenantProfile, ...} with the
	// SaaS identity shown on the desktop consent screen.
	Profiles string
	// CoreOnly runs a Core-only gateway (Config.CoreOnly; conformance testing).
	CoreOnly bool
	// Wrap, if set, wraps the gateway handler (e.g. to add the mock chat UI).
	Wrap func(http.Handler) http.Handler
}

func Serve(ctx context.Context, o ServeOptions) error {
	var c *store.Cipher
	if k := os.Getenv("LSP_DATA_KEY"); k != "" {
		kb, err := hex.DecodeString(k)
		if err != nil {
			return errors.New("LSP_DATA_KEY must be 64 hex characters")
		}
		if c, err = store.NewCipher(kb); err != nil {
			return err
		}
	}
	var sk ed25519.PrivateKey
	if k := os.Getenv("LSP_SIGNING_SEED"); k != "" {
		kb, err := hex.DecodeString(k)
		if err != nil || len(kb) != ed25519.SeedSize {
			return errors.New("LSP_SIGNING_SEED must be 64 hex characters")
		}
		sk = ed25519.NewKeyFromSeed(kb)
	} else if !o.AllowEphemeralKey {
		return errors.New("set LSP_SIGNING_SEED (devices pin the gateway key; an ephemeral key would force re-pairing on restart)")
	} else {
		log.Print("warning: ephemeral gateway signing key; paired devices must re-pair after restart")
	}
	st, err := store.OpenSQLite(o.DB, c)
	if err != nil {
		return err
	}
	defer st.Close()
	if o.Issuer == "" && !o.AllowEphemeralKey {
		return errors.New("set -issuer to the gateway's public https origin (tokens and metadata are bound to it)")
	}
	g, err := New(Config{Store: st, SigningKey: sk, Retention: o.Retention, Issuer: o.Issuer, CoreOnly: o.CoreOnly})
	if err != nil {
		return err
	}
	profiles := map[string]store.TenantProfile{}
	if o.Profiles != "" {
		b, err := os.ReadFile(o.Profiles)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &profiles); err != nil {
			return fmt.Errorf("tenant profiles: %w", err)
		}
	}
	for _, t := range strings.Split(o.Tenants, ",") {
		id, env, ok := strings.Cut(strings.TrimSpace(t), ":")
		if !ok || id == "" {
			return fmt.Errorf("bad tenant spec %q (want id:ENV_VAR)", t)
		}
		p, ok := profiles[id]
		if !ok {
			p = store.TenantProfile{DisplayName: id}
		}
		if err := g.RegisterTenantProfile(ctx, id, os.Getenv(env), p); err != nil {
			return fmt.Errorf("tenant %s: %w (set %s)", id, err, env)
		}
	}
	go g.RunRetention(ctx, 0)
	log.Printf("lmbp gateway listening on %s (gateway key %s, encryption at rest: %v)", o.Listen, lmbp.B64(g.PublicKey()), c != nil)
	h := g.Handler()
	if o.Wrap != nil {
		h = o.Wrap(h)
	}
	s := &http.Server{Addr: o.Listen, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); _ = s.Close() }()
	if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
