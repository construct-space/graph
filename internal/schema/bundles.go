package schema

import (
	"encoding/json"
	"net/http"
	"strings"
)

// HandleCreateSpaceBundle creates a new bundle owned by the caller's org.
// POST /api/space-bundles  { "id": "...", "name": "..." }
// Caller org is derived from the auth middleware's X-Auth-Org-ID header.
func (r *Registry) HandleCreateSpaceBundle(w http.ResponseWriter, req *http.Request) {
	callerOrgID := req.Header.Get("X-Auth-Org-ID")
	callerUserID := req.Header.Get("X-Auth-User-ID")

	owner := callerOrgID
	if owner == "" {
		owner = callerUserID
	}

	if owner == "" {
		http.Error(w, `{"error":"org or user context required (switch to an org scope)"}`, http.StatusForbidden)
		return
	}

	var body struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, 400)
		return
	}

	if err := r.CreateSpaceBundle(body.ID, body.Name, owner); err != nil {
		status := 400
		if strings.Contains(err.Error(), "already exists") {
			status = 409
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(201)
	json.NewEncoder(w).Encode(map[string]any{
		"ok":           true,
		"id":           body.ID,
		"name":         body.Name,
		"owner_org_id": callerOrgID,
	})
}

// HandleListSpaceBundles returns bundles owned by the caller's org.
// GET /api/space-bundles
func (r *Registry) HandleListSpaceBundles(w http.ResponseWriter, req *http.Request) {
	callerOrgID := req.Header.Get("X-Auth-Org-ID")
	callerUserID := req.Header.Get("X-Auth-User-ID")

	owner := callerOrgID
	if owner == "" {
		owner = callerUserID
	}

	if owner == "" {
		http.Error(w, `{"error":"org or user context required"}`, http.StatusForbidden)
		return
	}

	bundles, err := r.ListSpaceBundlesByOwner(owner)
	if err != nil {
		http.Error(w, `{"error":"list failed"}`, 500)
		return
	}
	if bundles == nil {
		bundles = []SpaceBundle{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"bundles": bundles})
}

// HandleGetSpaceBundle returns a single bundle (only if owned by caller's org).
// GET /api/space-bundles/{bundleId}
func (r *Registry) HandleGetSpaceBundle(w http.ResponseWriter, req *http.Request) {
	callerOrgID := req.Header.Get("X-Auth-Org-ID")
	callerUserID := req.Header.Get("X-Auth-User-ID")

	owner := callerOrgID
	if owner == "" {
		owner = callerUserID
	}

	if owner == "" {
		http.Error(w, `{"error":"org or user context required"}`, http.StatusForbidden)
		return
	}
	id := req.PathValue("bundleId")
	b, err := r.GetSpaceBundle(id)
	if err != nil {
		http.Error(w, `{"error":"lookup failed"}`, 500)
		return
	}
	if b == nil {
		http.Error(w, `{"error":"not found"}`, 404)
		return
	}
	if b.OwnerOrgID != owner {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(b)
}
