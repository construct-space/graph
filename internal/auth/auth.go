package auth

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	goauth "github.com/construct-space/go-auth"
)

// User represents an authenticated user from accounts service
type User struct {
	ID              json.Number `json:"id"`
	UUID            string      `json:"uuid"`
	Email           string      `json:"email"`
	Name            string      `json:"name"`
	AvatarURL       string      `json:"avatar_url"`
	DeveloperStatus string      `json:"developer_status"` // legacy; prefer Scope/Developer fields

	// Scope-aware identity — populated from accounts /api/me/scope.
	Scope     string   `json:"scope"`     // "user" | "org"
	OrgID     string   `json:"org_id"`    // set when Scope == "org"
	OrgSlug   string   `json:"org_slug"`  // set when Scope == "org"
	Roles     []string `json:"roles"`     // set when Scope == "org"
	Developer bool     `json:"developer"` // set when Scope == "user"
}

// IDString returns the user ID as a string
func (u *User) IDString() string {
	return u.ID.String()
}

// IsDeveloper returns true if the user has developer capability.
// Personal scope: the Developer flag from accounts /me/scope.
// Org scope: the "developer" role assigned to them in the org.
// Falls back to legacy DeveloperStatus for callers still using /api/me.
func (u *User) IsDeveloper() bool {
	if u.Scope == "user" && u.Developer {
		return true
	}
	if u.Scope == "org" && slices.Contains(u.Roles, "developer") {
		return true
	}
	return u.DeveloperStatus == "enrolled"
}

// HasRole reports whether the user holds the given role in their current
// org scope. Returns false for personal scope.
func (u *User) HasRole(role string) bool {
	if u.Scope != "org" {
		return false
	}
	return slices.Contains(u.Roles, role)
}

// Publisher represents a developer from the dev-portal. Identity anchor —
// exactly one of UserID (personal) or OrgID (org) is non-empty. We surface
// both so RequireAuth can write the matching X-Auth-User-ID / X-Auth-Org-ID
// header and the schema registry's ownership checks treat publisher-key
// auth as either a user push or an org push.
type Publisher struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Verified bool   `json:"verified"`
	UserID   string `json:"user_id"`
	OrgID    string `json:"org_id"`
	Kind     string `json:"kind"`
}

// AuthContext holds the authenticated identity for a request
type AuthContext struct {
	User      *User
	Publisher *Publisher
	Token     string
}

// Middleware validates auth tokens against accounts and dev-portal services
type Middleware struct {
	accountsURL  string // https://accounts.lisaos.dev
	devPortalURL string // https://developer.lisaos.dev

	// internalSecret matches the my.lisaos.dev gateway's
	// X-Internal-Secret header. When set + present on a request, the
	// gateway has already run accounts' auth_request and written attested
	// X-Auth-* headers — we trust those instead of re-validating a bearer
	// token that the browser session didn't send.
	internalSecret string

	// Simple token cache (token → user, expiry)
	cache   map[string]*cacheEntry
	cacheMu sync.RWMutex
}

type cacheEntry struct {
	ctx     *AuthContext
	expires time.Time
}

func NewMiddleware(accountsURL, devPortalURL string) *Middleware {
	return &Middleware{
		accountsURL:  accountsURL,
		devPortalURL: devPortalURL,
		cache:        make(map[string]*cacheEntry),
	}
}

// SetInternalSecret configures the shared secret used by the gateway
// (my.lisaos.dev). Incoming requests carrying a matching
// X-Internal-Secret skip the bearer re-validation and trust the X-Auth-*
// headers the gateway already attested via accounts /internal/validate-token.
func (m *Middleware) SetInternalSecret(secret string) {
	m.internalSecret = secret
}

// authContextFromGateway builds the graph-local AuthContext shape from the
// gateway-asserted identity. The ID field is stored as json.Number for
// legacy reasons (downstream code formats it via String()); the UUID is the
// actual identifier in the unified world. We set both so either reader sees
// the right value.
func authContextFromGateway(id goauth.Identity) *AuthContext {
	return &AuthContext{
		User: &User{
			ID:      json.Number(id.UserID),
			UUID:    id.UserID,
			Email:   id.Email,
			Name:    id.Name,
			Scope:   id.Scope,
			OrgID:   id.OrgID,
			OrgSlug: id.OrgSlug,
			Roles:   id.Roles,
		},
	}
}

// Authenticate validates the request and returns auth context.
//
// When the request comes through the my.lisaos.dev gateway, the gateway
// has already validated the token against accounts and written X-Auth-*
// headers — we short-circuit to those headers and skip the HTTP round-trip
// to accounts. For direct calls (dev, curl, construct-app bypassing the
// gateway) we fall through to the full validation flow.
func (m *Middleware) Authenticate(r *http.Request) (*AuthContext, error) {
	if id := goauth.Gateway(r, m.internalSecret); id.Authenticated() {
		return authContextFromGateway(id), nil
	}

	// Check Authorization header
	authHeader := r.Header.Get("Authorization")
	apiKey := r.Header.Get("X-API-Key")

	if authHeader != "" {
		token := strings.TrimPrefix(authHeader, "Bearer ")

		// Check cache
		if ctx := m.fromCache(token); ctx != nil {
			return ctx, nil
		}

		// cat_ prefix → accounts service OAuth token
		if strings.HasPrefix(token, "cat_") {
			return m.validateAccountsToken(token)
		}

		// cst_live_ prefix → dev-portal CLI token
		if strings.HasPrefix(token, "cst_live_") {
			return m.validateCLIToken(token)
		}

		// Try accounts service for any other bearer token
		return m.validateAccountsToken(token)
	}

	if apiKey != "" {
		// Check cache
		if ctx := m.fromCache(apiKey); ctx != nil {
			return ctx, nil
		}

		// csk_live_ prefix → dev-portal publisher API key
		if strings.HasPrefix(apiKey, "csk_live_") {
			return m.validatePublisherKey(apiKey)
		}
	}

	// Fall back to session cookie (set by OAuth login on this domain)
	if s := GetSession(r); s != nil {
		return &AuthContext{
			User: &User{
				ID:    json.Number(s.UserID),
				Email: s.Email,
				Name:  s.Name,
			},
		}, nil
	}

	return nil, fmt.Errorf("no authentication provided")
}

// RequireAuth is HTTP middleware that rejects unauthenticated requests
func (m *Middleware) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Gateway-signed request — my.lisaos.dev already ran auth_request
		// against accounts /internal/validate-token and wrote X-Auth-* headers
		// from the verified session. Skip bearer re-validation AND skip the
		// blanket X-Auth-* strip below, which would nuke the gateway's
		// attestation and leave the request unauthenticated.
		if m.internalSecret != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Internal-Secret")), []byte(m.internalSecret)) == 1 {
			next.ServeHTTP(w, r)
			return
		}

		authCtx, err := m.Authenticate(r)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized: " + err.Error()})
			return
		}
		// SECURITY: wipe every X-Auth-* header from the client before we
		// write the attested values. Without this, a non-gateway caller
		// could POST with X-Auth-Org-ID: <victim-org> and the conditional
		// 'set only when attested OrgID is non-empty' branch below would
		// leave the forged header intact — cross-org hijack on bundle
		// attach, install, allowlist, and every publisher_admin check.
		for key := range r.Header {
			if strings.HasPrefix(key, "X-Auth-") {
				r.Header.Del(key)
			}
		}

		// Store auth context in headers for downstream use
		if authCtx.User != nil {
			r.Header.Set("X-Auth-User-ID", authCtx.User.IDString())
			if authCtx.User.UUID != "" {
				r.Header.Set("X-Auth-User-UUID", authCtx.User.UUID)
			}
			r.Header.Set("X-Auth-User-Email", authCtx.User.Email)
			r.Header.Set("X-Auth-User-Name", authCtx.User.Name)
			r.Header.Set("X-Auth-Developer-Status", authCtx.User.DeveloperStatus)
			// Scope-aware headers (resolvers can read these without re-parsing).
			if authCtx.User.Scope != "" {
				r.Header.Set("X-Auth-Scope", authCtx.User.Scope)
			}
			if authCtx.User.OrgID != "" {
				r.Header.Set("X-Auth-Org-ID", authCtx.User.OrgID)
				r.Header.Set("X-Auth-Org-Slug", authCtx.User.OrgSlug)
			}
			if len(authCtx.User.Roles) > 0 {
				r.Header.Set("X-Auth-Roles", strings.Join(authCtx.User.Roles, ","))
			}
		}
		if authCtx.Publisher != nil {
			r.Header.Set("X-Auth-Publisher", authCtx.Publisher.Name)
			r.Header.Set("X-Auth-Developer-Status", "enrolled") // publishers are developers
			// Surface the publisher's owning identity as the attested caller so
			// downstream handlers (schema registry ownership, bundle attach,
			// install gating) treat a publisher-key call as either a user push
			// or an org push depending on which anchor the publisher row has.
			// Only fills slots not already set by an accompanying user token.
			if authCtx.Publisher.OrgID != "" && r.Header.Get("X-Auth-Org-ID") == "" {
				r.Header.Set("X-Auth-Org-ID", authCtx.Publisher.OrgID)
				r.Header.Set("X-Auth-Scope", "org")
			}
			if authCtx.Publisher.UserID != "" && r.Header.Get("X-Auth-User-ID") == "" {
				r.Header.Set("X-Auth-User-ID", authCtx.Publisher.UserID)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// validateAccountsToken calls accounts /api/me/scope with the token and
// populates the scope-aware User fields (Scope, OrgID, Roles, Developer).
// accountsURL is the service root (e.g. http://srv-captain--accounts);
// the accounts service serves its API under /api/.
// Cached for 5 minutes — mid-session scope changes take effect on app
// restart, matching the product decision.
func (m *Middleware) validateAccountsToken(token string) (*AuthContext, error) {
	req, _ := http.NewRequest("GET", m.accountsURL+"/api/me/scope", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("accounts service unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("invalid token (status %d)", resp.StatusCode)
	}

	// /api/me/scope shape: { authenticated, user, scope, org?, roles?, developer? }
	var body struct {
		User  *User  `json:"user"`
		Scope string `json:"scope"`
		Org   *struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
		} `json:"org"`
		Roles     []string `json:"roles"`
		Developer bool     `json:"developer"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("invalid scope response: %w", err)
	}
	if body.User == nil {
		return nil, fmt.Errorf("scope response missing user")
	}

	user := *body.User
	user.Scope = body.Scope
	user.Roles = body.Roles
	user.Developer = body.Developer
	if body.Org != nil {
		user.OrgID = body.Org.ID
		user.OrgSlug = body.Org.Slug
	}
	// Accounts /me/scope returns uuid but not id; fall back so downstream
	// headers (X-Auth-User-ID) and ownership checks have a value.
	if user.ID == "" && user.UUID != "" {
		user.ID = json.Number(user.UUID)
	}

	ctx := &AuthContext{User: &user, Token: token}
	m.toCache(token, ctx, 5*time.Minute)
	return ctx, nil
}

// validateCLIToken calls dev-portal to verify CLI token
func (m *Middleware) validateCLIToken(token string) (*AuthContext, error) {
	req, _ := http.NewRequest("GET", m.devPortalURL+"/api/auth/cli-verify", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dev-portal unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("invalid CLI token (status %d)", resp.StatusCode)
	}

	var result struct {
		Valid bool   `json:"valid"`
		Email string `json:"email"`
		Name  string `json:"name"`
		User  *struct {
			ID    string `json:"id"`
			Email string `json:"email"`
			Name  string `json:"name"`
		} `json:"user"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	// Support both formats: {valid, email, name} and {user: {id, email, name}}
	email := result.Email
	name := result.Name
	userID := ""
	if result.User != nil {
		email = result.User.Email
		name = result.User.Name
		userID = result.User.ID
	} else if !result.Valid {
		return nil, fmt.Errorf("CLI token not valid")
	}

	if email == "" {
		return nil, fmt.Errorf("CLI token not valid")
	}

	ctx := &AuthContext{
		User:  &User{ID: json.Number(userID), Email: email, Name: name, DeveloperStatus: "enrolled"},
		Token: token,
	}
	m.toCache(token, ctx, 5*time.Minute)
	return ctx, nil
}

// validatePublisherKey validates a publisher API key against dev-portal
func (m *Middleware) validatePublisherKey(key string) (*AuthContext, error) {
	req, _ := http.NewRequest("GET", m.devPortalURL+"/api/auth/verify-key", nil)
	req.Header.Set("X-API-Key", key)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dev-portal unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("invalid API key (status %d)", resp.StatusCode)
	}

	var pub Publisher
	json.NewDecoder(resp.Body).Decode(&pub)

	ctx := &AuthContext{Publisher: &pub, Token: key}
	m.toCache(key, ctx, 10*time.Minute)
	return ctx, nil
}

func (m *Middleware) fromCache(key string) *AuthContext {
	m.cacheMu.RLock()
	defer m.cacheMu.RUnlock()
	if entry, ok := m.cache[key]; ok && time.Now().Before(entry.expires) {
		return entry.ctx
	}
	return nil
}

func (m *Middleware) toCache(key string, ctx *AuthContext, ttl time.Duration) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	m.cache[key] = &cacheEntry{ctx: ctx, expires: time.Now().Add(ttl)}
}
