package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"construct-graph/internal/auth"
	"construct-graph/internal/engine"
	"construct-graph/internal/graphql"
	"construct-graph/internal/schema"
)

func main() {
	devModeRequested := os.Getenv("DEV_MODE") == "true"
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		if !devModeRequested {
			log.Fatal("DATABASE_URL is required unless DEV_MODE=true")
		}
		dbURL = "data/graph.db" // SQLite default for local dev
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	accountsURL := os.Getenv("ACCOUNTS_URL")
	if accountsURL == "" {
		accountsURL = "http://srv-captain--accounts"
	}
	devPortalURL := os.Getenv("DEV_PORTAL_URL")
	if devPortalURL == "" {
		devPortalURL = "http://srv-captain--developer"
	}

	// Connect to database (PostgreSQL or SQLite)
	db, err := engine.Connect(dbURL)
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	sqlDB, _ := db.DB()
	if sqlDB != nil {
		defer sqlDB.Close()
	}

	// Initialize system schema
	registry := schema.NewRegistry(db)
	if err := registry.InitSystem(); err != nil {
		log.Fatalf("failed to initialize system schema: %v", err)
	}

	// Create query engine
	eng := engine.New(db)
	eventHub := graphql.NewEventHub()
	eng.SetEventSink(eventHub.Publish)

	internalSecret := os.Getenv("INTERNAL_SHARED_SECRET")
	if internalSecret == "" {
		log.Printf("warn: INTERNAL_SHARED_SECRET is unset — my.lisaos.dev gateway requests will 401. Set it to the same value configured on the my app.")
	} else {
		log.Printf("info: INTERNAL_SHARED_SECRET configured (len=%d) — gateway bypass enabled", len(internalSecret))
	}

	// Dev mode skips auth only when explicitly requested. Production must not
	// become unauthenticated just because DATABASE_URL was omitted or SQLite was
	// selected accidentally.
	devMode := devModeRequested

	// Auth middleware — validates tokens from accounts + dev-portal.
	// internalSecret, when set, lets requests from the my.lisaos.dev
	// gateway (which carries X-Internal-Secret + pre-attested X-Auth-*
	// headers from accounts /internal/validate-token) bypass bearer
	// re-validation. Without this the gateway path loses auth context when
	// the middleware strips all X-Auth-* headers and has no bearer to
	// attest from (browser sessions don't carry one).
	authMw := auth.NewMiddleware(accountsURL, devPortalURL)
	authMw.SetInternalSecret(internalSecret)

	// requireAuth wraps a handler with auth in production, skips in dev mode
	requireAuth := func(next http.Handler) http.Handler {
		if devMode {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Auth-User-ID") == "" {
					r.Header.Set("X-Auth-User-ID", "dev-user")
					r.Header.Set("X-Auth-User-Email", "dev@localhost")
				}
				next.ServeHTTP(w, r)
			})
		}
		return authMw.RequireAuth(next)
	}

	// requireAdminAuth allows requests authenticated via X-Internal-Secret (oracle proxy)
	// or via the normal OAuth session/token path. Admin routes use this so oracle can
	// call them without a user session while existing browser-based admin flows still work.
	requireAdminAuth := func(next http.Handler) http.Handler {
		if devMode {
			return requireAuth(next)
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if internalSecret != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Internal-Secret")), []byte(internalSecret)) == 1 {
				r.Header.Set("X-Auth-User-ID", "oracle-admin")
				r.Header.Set("X-Auth-User-Email", "oracle@internal")
				next.ServeHTTP(w, r)
				return
			}
			authMw.RequireAuth(next).ServeHTTP(w, r)
		})
	}

	// Create GraphQL handler
	gqlHandler := graphql.NewHandler(registry, eng)
	gqlHandler.SetEventHub(eventHub)

	// OAuth config
	oauthCfg := auth.LoadOAuthConfig()

	// HTTP server
	mux := http.NewServeMux()

	// OAuth auth routes
	mux.HandleFunc("GET /api/auth/login", auth.LoginRedirect(oauthCfg))
	mux.HandleFunc("GET /api/auth/callback", auth.AuthCallback(oauthCfg))
	mux.HandleFunc("GET /api/auth/me", auth.AuthMe)
	mux.HandleFunc("GET /api/auth/logout", auth.AuthLogout(oauthCfg))

	// Health check (public)
	healthHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok","service":"construct-graph"}`))
	}
	mux.HandleFunc("GET /health", healthHandler)
	mux.HandleFunc("GET /api/health", healthHandler)

	// GraphQL endpoint
	mux.Handle("POST /graphql", corsMiddleware(requireAuth(gqlHandler)))
	mux.Handle("OPTIONS /graphql", corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	// GraphQL playground
	mux.Handle("GET /graphql", corsMiddleware(graphql.PlaygroundHandler()))

	// Realtime event stream
	mux.Handle("GET /realtime/stream", corsMiddleware(requireAuth(http.HandlerFunc(gqlHandler.ServeRealtime))))
	mux.Handle("OPTIONS /realtime/stream", corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))

	// CORS preflight for API routes
	mux.Handle("OPTIONS /api/", corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))

	// Schema registration
	mux.Handle("POST /api/schemas/register", corsMiddleware(requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry.HandleRegister(w, r)
	}))))

	// Schema info
	mux.Handle("GET /api/schemas/{spaceId}", corsMiddleware(requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry.HandleGetSchema(w, r)
	}))))

	// Schema deletion
	mux.Handle("DELETE /api/schemas/{spaceId}", corsMiddleware(requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry.HandleDeleteSchema(w, r)
	}))))

	// Publisher dashboard: list spaces owned by caller org.
	mux.Handle("GET /api/spaces", corsMiddleware(requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry.HandleListSpaces(w, r)
	}))))

	// Space bundles — publisher-side grouping. All three routes require org
	// context (X-Auth-Org-ID) set by the auth middleware.
	mux.Handle("POST /api/space-bundles", corsMiddleware(requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry.HandleCreateSpaceBundle(w, r)
	}))))
	mux.Handle("GET /api/space-bundles", corsMiddleware(requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry.HandleListSpaceBundles(w, r)
	}))))
	mux.Handle("GET /api/space-bundles/{bundleId}", corsMiddleware(requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry.HandleGetSpaceBundle(w, r)
	}))))

	// Installs + distribution (Phase 4). Tenants install spaces they want to
	// use; publishers manage distribution + allowlist.
	mux.Handle("POST /api/spaces/{spaceId}/install", corsMiddleware(requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry.HandleInstallSpace(w, r)
	}))))
	mux.Handle("DELETE /api/spaces/{spaceId}/install", corsMiddleware(requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry.HandleUninstallSpace(w, r)
	}))))
	mux.Handle("GET /api/spaces/{spaceId}/installs", corsMiddleware(requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry.HandleListInstalls(w, r)
	}))))
	mux.Handle("PUT /api/spaces/{spaceId}/distribution", corsMiddleware(requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry.HandleSetDistribution(w, r)
	}))))
	mux.Handle("POST /api/spaces/{spaceId}/allowlist", corsMiddleware(requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry.HandleAddAllowlist(w, r)
	}))))
	mux.Handle("DELETE /api/spaces/{spaceId}/allowlist", corsMiddleware(requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry.HandleRemoveAllowlist(w, r)
	}))))

	// Admin endpoints — accept X-Internal-Secret (oracle proxy) or OAuth session
	mux.Handle("GET /api/admin/stats", corsMiddleware(requireAdminAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry.AdminStats(w, r)
	}))))
	mux.Handle("GET /api/admin/schemas/{spaceId}/models", corsMiddleware(requireAdminAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry.AdminGetModels(w, r)
	}))))
	mux.Handle("GET /api/admin/schemas", corsMiddleware(requireAdminAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry.AdminListSchemas(w, r)
	}))))
	mux.Handle("DELETE /api/admin/schemas/{name}", corsMiddleware(requireAdminAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry.AdminDeleteSchema(w, r)
	}))))
	// Browse rows from a single table inside a provisioned schema. Backs
	// Oracle's data drawer + CSV export.
	mux.Handle("GET /api/admin/schemas/{schemaName}/tables/{tableName}/rows", corsMiddleware(requireAdminAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry.AdminGetTableRows(w, r)
	}))))
	// Owner-scoped row browse — keyed by space id, ownership checked
	// against the X-Auth-* headers the gateway / developer façade sets.
	// Backs my.lisaos.dev's developer data drawer.
	mux.Handle("GET /api/spaces/{spaceId}/tables/{tableName}/rows", corsMiddleware(requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry.HandleGetTableRows(w, r)
	}))))
	mux.Handle("GET /api/admin/spaces", corsMiddleware(requireAdminAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry.AdminListSpaces(w, r)
	}))))
	// Admin space cascade-delete: drops all schemas + manifests + the spaces
	// row for a single space id. Used by Oracle to clean up orphan / never-
	// submitted spaces. Reuses HandleDeleteSchema (which already does the
	// cascade); the only thing different from /api/schemas/{spaceId} is the
	// auth gate — admin path accepts X-Internal-Secret, the /api/schemas
	// route requires a user session.
	mux.Handle("DELETE /api/admin/spaces/{spaceId}", corsMiddleware(requireAdminAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry.HandleDeleteSchema(w, r)
	}))))

	// Root redirect — graph is API-only; send browser traffic to the portal.
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://my.lisaos.dev", http.StatusFound)
	})

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(ctx)
	}()

	fmt.Printf("construct-graph listening on :%s (endpoints: /graphql, /health, /api/*)\n", port)
	fmt.Printf("  database: %s\n", dbURL)
	if devMode {
		fmt.Printf("  mode: DEV (auth disabled, playground open)\n")
	} else {
		fmt.Printf("  mode: PRODUCTION\n")
		fmt.Printf("  accounts: %s\n", accountsURL)
		fmt.Printf("  dev-portal: %s\n", devPortalURL)
	}
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key, X-Space-ID, X-Project-ID, X-Company-ID, X-Auth-User-ID, X-Auth-Developer-Status")
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}
