package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/awslabs/aws-lambda-go-api-proxy/httpadapter"
)

func main() {
	cfg := LoadConfig()

	// Initialize store based on mode
	var store DataStore
	var err error

	switch cfg.StoreMode {
	case "dynamo":
		store, err = NewDynamoStore(context.Background(), cfg)
		if err != nil {
			log.Fatalf("failed to create DynamoDB store: %v", err)
		}
	default:
		store, err = NewSQLiteStore(cfg.DatabasePath)
		if err != nil {
			log.Fatalf("failed to open SQLite database: %v", err)
		}
	}
	defer store.Close()

	s3client, err := NewS3Client(context.Background(), cfg)
	if err != nil {
		log.Fatalf("failed to create S3 client: %v", err)
	}

	handler := buildHandler(store, s3client, cfg)

	// Lambda mode: use the API Gateway v2 adapter
	if cfg.IsLambda() {
		log.Println("starting in Lambda mode")
		lambda.Start(httpadapter.NewV2(handler).ProxyWithContext)
		return
	}

	// HTTP server mode (local dev)
	srv := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("shutting down...")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.Printf("backup service listening on %s (store: %s)", cfg.ListenAddr, cfg.StoreMode)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}

func buildHandler(store DataStore, s3client *S3Client, cfg *Config) http.Handler {
	h := &Handlers{
		store:  store,
		s3:     s3client,
		config: cfg,
	}

	mux := http.NewServeMux()

	// Public (rate-limited, open registration)
	mux.Handle("POST /v1/agents/register", RateLimit(cfg.RegisterRateLimit, http.HandlerFunc(h.Register)))

	// Authenticated + RequireActive (mutation endpoints)
	mux.Handle("POST /v1/backups/upload-url", Auth(store, RequireActive(http.HandlerFunc(h.UploadURL))))
	mux.Handle("DELETE /v1/backups", Auth(store, RequireActive(http.HandlerFunc(h.DeleteAllBackups))))
	mux.Handle("DELETE /v1/backups/{timestamp}", Auth(store, RequireActive(http.HandlerFunc(h.DeleteBackup))))
	mux.Handle("POST /v1/backups/{timestamp}/undelete", Auth(store, RequireActive(http.HandlerFunc(h.UndeleteBackup))))

	// Authenticated (read endpoints — pending/suspended agents can still use these)
	mux.Handle("GET /v1/backups", Auth(store, http.HandlerFunc(h.ListBackups)))
	mux.Handle("GET /v1/backups/{timestamp}", Auth(store, http.HandlerFunc(h.GetBackup)))
	mux.Handle("POST /v1/backups/download-url", Auth(store, http.HandlerFunc(h.DownloadURL)))

	// Agent management (auth-only, no active requirement)
	mux.Handle("GET /v1/agents/me", Auth(store, http.HandlerFunc(h.AgentInfo)))
	mux.Handle("POST /v1/agents/me/rotate-token", Auth(store, http.HandlerFunc(h.RotateToken)))

	// Admin endpoints (protected by X-API-Key header)
	mux.Handle("GET /v1/admin/agents", APIKeyAuth(cfg.AdminAPIKey, http.HandlerFunc(h.AdminListAgents)))
	mux.Handle("POST /v1/admin/agents/{id}/approve", APIKeyAuth(cfg.AdminAPIKey, http.HandlerFunc(h.AdminApproveAgent)))
	mux.Handle("POST /v1/admin/agents/{id}/suspend", APIKeyAuth(cfg.AdminAPIKey, http.HandlerFunc(h.AdminSuspendAgent)))

	// Health
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	return LogRequests(mux)
}
