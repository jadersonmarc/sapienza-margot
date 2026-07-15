package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jadersonmarc/sapienza-kit/authclient"
	"github.com/jadersonmarc/sapienza-kit/gating"

	"github.com/jadersonmarc/sapienza-margot/db/migrations"
	"github.com/jadersonmarc/sapienza-margot/internal/agent"
	"github.com/jadersonmarc/sapienza-margot/internal/api"
	"github.com/jadersonmarc/sapienza-margot/internal/channel"
	"github.com/jadersonmarc/sapienza-margot/internal/claude"
	"github.com/jadersonmarc/sapienza-margot/internal/db"
	"github.com/jadersonmarc/sapienza-margot/internal/pipeline"
	"github.com/jadersonmarc/sapienza-margot/internal/provisioning"
	"github.com/jadersonmarc/sapienza-margot/internal/secrets"
	"github.com/jadersonmarc/sapienza-margot/internal/whatsapp"
)

func main() {
	ctx := context.Background()
	dsn := mustEnv("DATABASE_URL")

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	defer pool.Close()

	// Product-global `margot` schema (routing/config/secrets).
	if err := db.MigrateMargot(ctx, pool, migrations.MargotSchema); err != nil {
		log.Fatalf("migrate margot: %v", err)
	}

	// Optional per-tenant secret cipher.
	var cipher *secrets.Cipher
	if key := os.Getenv("MARGOT_ENC_KEY"); key != "" {
		if cipher, err = secrets.New(key); err != nil {
			log.Fatalf("secrets: %v", err)
		}
	}

	// Channel resolution (Evolution instance → tenant).
	resolver := channel.NewResolver(channel.NewLoader(pool, cipher))

	// Evolution sender (global server; instance routes per tenant).
	sender := whatsapp.NewClient(os.Getenv("EVOLUTION_API_URL"), os.Getenv("EVOLUTION_API_KEY"))

	// Claude replier (fallback-only if no key).
	var replier agent.Replier
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		replier = claude.NewReplier(key)
	} else {
		log.Println("ANTHROPIC_API_KEY not set — bot replies with tenant fallback only")
	}

	gate := gating.New(pool)
	pipe := pipeline.New(pool, sender, replier, gate)
	webhook := whatsapp.NewHandler(resolver, pipe, os.Getenv("EVOLUTION_WEBHOOK_SECRET"))

	verifier := authclient.NewVerifier([]byte(mustEnv("PRODUCT_JWT_SECRET")), "sapienza-core")
	apiServer := api.NewServer(pool, verifier, gate, sender, cipher)

	// Provisioning: apply tenant migrations on SubscriptionActivated{margot}.
	listener := provisioning.New(pool)
	if err := listener.CatchUp(ctx); err != nil {
		log.Printf("provisioning catch-up: %v", err)
	}
	go listener.Run(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.Handle("/webhook/evolution", webhook)
	mux.Handle("/api/v1/", apiServer.Handler())

	addr := ":" + getenv("PORT", "8081")
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second, // replies call Claude synchronously
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("margot running on %s", addr)
	log.Fatal(srv.ListenAndServe())
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("missing required env %s", key)
	}
	return v
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
