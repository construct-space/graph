package auth

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// parseUserUUID extracts the id field from accounts /api/me as a UUID string.
// accounts now always emits "id" as a UUID string. Returns ("", false) when
// the field is absent or blank so the caller can surface a distinct error.
func parseUserUUID(raw any) (string, bool) {
	switch v := raw.(type) {
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return "", false
		}
		return s, true
	default:
		return "", false
	}
}

// OAuth config
type OAuthConfig struct {
	URL          string
	ClientID     string
	ClientSecret string
	RedirectURI  string
	AppURL       string
}

func LoadOAuthConfig() *OAuthConfig {
	return &OAuthConfig{
		URL:          envOr("OAUTH_URL", "https://accounts.lisaos.dev"),
		ClientID:     os.Getenv("OAUTH_CLIENT_ID"),
		ClientSecret: os.Getenv("OAUTH_CLIENT_SECRET"),
		RedirectURI:  envOr("OAUTH_REDIRECT_URI", "https://graph.lisaos.dev/api/auth/callback"),
		AppURL:       envOr("APP_URL", "https://graph.lisaos.dev"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Session management
var (
	sessionsMu sync.RWMutex
	sessions   = make(map[string]*SessionData)
	statesMu   sync.Mutex
	states     = make(map[string]time.Time)
)

type SessionData struct {
	UserID    string
	Name      string
	Email     string
	ExpiresAt time.Time
}

func generateState() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func generateSessionToken() string {
	b := make([]byte, 48)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// LoginRedirect redirects to Construct OAuth
func LoginRedirect(cfg *OAuthConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		state := generateState()

		statesMu.Lock()
		states[state] = time.Now().Add(10 * time.Minute)
		now := time.Now()
		for k, exp := range states {
			if now.After(exp) {
				delete(states, k)
			}
		}
		statesMu.Unlock()

		params := url.Values{
			"client_id":     {cfg.ClientID},
			"redirect_uri":  {cfg.RedirectURI},
			"response_type": {"code"},
			"scope":         {"profile email"},
			"state":         {state},
		}

		http.Redirect(w, r, cfg.URL+"/oauth/authorize?"+params.Encode(), http.StatusFound)
	}
}

// AuthCallback handles OAuth callback
func AuthCallback(cfg *OAuthConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		state := r.URL.Query().Get("state")

		if code == "" {
			http.Redirect(w, r, cfg.AppURL+"/?error=missing_code", http.StatusFound)
			return
		}

		statesMu.Lock()
		exp, exists := states[state]
		if exists {
			delete(states, state)
		}
		statesMu.Unlock()

		if !exists || time.Now().After(exp) {
			http.Redirect(w, r, cfg.AppURL+"/?error=invalid_state", http.StatusFound)
			return
		}

		// Exchange code for token
		tokenData, err := exchangeCode(cfg, code)
		if err != nil {
			http.Redirect(w, r, cfg.AppURL+"/?error=token_exchange", http.StatusFound)
			return
		}

		accessToken, _ := tokenData["access_token"].(string)
		if accessToken == "" {
			http.Redirect(w, r, cfg.AppURL+"/?error=no_token", http.StatusFound)
			return
		}

		// Fetch user info
		userInfo, err := fetchUserInfo(cfg, accessToken)
		if err != nil {
			http.Redirect(w, r, cfg.AppURL+"/?error=user_info", http.StatusFound)
			return
		}

		// accounts /api/me now returns `id` as a UUID string.
		userID, idOK := parseUserUUID(userInfo["id"])
		if !idOK {
			http.Redirect(w, r, cfg.AppURL+"/?error=bad_user", http.StatusFound)
			return
		}

		name, _ := userInfo["name"].(string)
		email, _ := userInfo["email"].(string)
		if name == "" {
			first, _ := userInfo["first_name"].(string)
			last, _ := userInfo["last_name"].(string)
			name = strings.TrimSpace(first + " " + last)
		}

		// Create session
		token := generateSessionToken()
		sessionsMu.Lock()
		sessions[token] = &SessionData{
			UserID:    userID,
			Name:      name,
			Email:     email,
			ExpiresAt: time.Now().Add(30 * 24 * time.Hour),
		}
		sessionsMu.Unlock()

		secure := strings.HasPrefix(cfg.AppURL, "https")
		http.SetCookie(w, &http.Cookie{
			Name:     "session",
			Value:    token,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   secure,
			MaxAge:   30 * 24 * 60 * 60,
		})

		http.Redirect(w, r, cfg.AppURL, http.StatusFound)
	}
}

// AuthMe returns current user
func AuthMe(w http.ResponseWriter, r *http.Request) {
	s := GetSession(r)
	if s == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		w.Write([]byte(`{"authenticated":false}`))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"authenticated": true,
		"user": map[string]any{
			"id":    s.UserID,
			"name":  s.Name,
			"email": s.Email,
		},
	})
}

// AuthLogout clears session
func AuthLogout(cfg *OAuthConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("session"); err == nil {
			sessionsMu.Lock()
			delete(sessions, c.Value)
			sessionsMu.Unlock()
		}

		http.SetCookie(w, &http.Cookie{
			Name:   "session",
			Value:  "",
			Path:   "/",
			MaxAge: -1,
		})

		http.Redirect(w, r, cfg.AppURL, http.StatusFound)
	}
}

// GetSession returns session from cookie
func GetSession(r *http.Request) *SessionData {
	c, err := r.Cookie("session")
	if err != nil {
		return nil
	}

	sessionsMu.RLock()
	s, ok := sessions[c.Value]
	sessionsMu.RUnlock()

	if !ok || time.Now().After(s.ExpiresAt) {
		return nil
	}
	return s
}

func exchangeCode(cfg *OAuthConfig, code string) (map[string]any, error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"client_id":     cfg.ClientID,
		"client_secret": cfg.ClientSecret,
		"redirect_uri":  cfg.RedirectURI,
	})

	resp, err := http.Post(cfg.URL+"/oauth/token", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("token exchange failed: %d", resp.StatusCode)
	}
	return result, nil
}

func fetchUserInfo(cfg *OAuthConfig, accessToken string) (map[string]any, error) {
	req, _ := http.NewRequest("GET", cfg.URL+"/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("user info failed: %d", resp.StatusCode)
	}
	return result, nil
}
