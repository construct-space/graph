package schema

import (
	"encoding/json"
	"net/http"
	"strings"
)

// HandleInstallSpace registers the caller's org as an installer of the space.
// POST /api/spaces/{spaceId}/install
func (r *Registry) HandleInstallSpace(w http.ResponseWriter, req *http.Request) {
	orgID := req.Header.Get("X-Auth-Org-ID")
	if orgID == "" {
		http.Error(w, `{"error":"org context required"}`, http.StatusForbidden)
		return
	}
	spaceID := req.PathValue("spaceId")
	if spaceID == "" {
		http.Error(w, `{"error":"space id required"}`, 400)
		return
	}
	if err := r.InstallSpace(spaceID, orgID); err != nil {
		status := 403
		if strings.Contains(err.Error(), "unknown distribution") {
			status = 500
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "space_id": spaceID, "org_id": orgID})
}

// HandleUninstallSpace removes the caller's org from the installer list.
// DELETE /api/spaces/{spaceId}/install
func (r *Registry) HandleUninstallSpace(w http.ResponseWriter, req *http.Request) {
	orgID := req.Header.Get("X-Auth-Org-ID")
	if orgID == "" {
		http.Error(w, `{"error":"org context required"}`, http.StatusForbidden)
		return
	}
	spaceID := req.PathValue("spaceId")
	if err := r.UninstallSpace(spaceID, orgID); err != nil {
		http.Error(w, `{"error":"uninstall failed"}`, 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// HandleListInstalls returns the orgs that installed a space. Gated to the
// space's publisher org (reading this is a publisher-admin concern).
// GET /api/spaces/{spaceId}/installs
func (r *Registry) HandleListInstalls(w http.ResponseWriter, req *http.Request) {
	orgID := req.Header.Get("X-Auth-Org-ID")
	userID := req.Header.Get("X-Auth-User-ID")
	spaceID := req.PathValue("spaceId")

	if orgID != "" {
		publisher := r.GetSpacePublisherOrg(spaceID)
		if publisher == "" || publisher != orgID {
			http.Error(w, `{"error":"forbidden: not the publisher org"}`, http.StatusForbidden)
			return
		}
	} else if userID != "" {
		owner := r.GetSpaceOwnerUser(spaceID)
		if owner == "" || owner != userID {
			http.Error(w, `{"error":"forbidden: not the publisher user"}`, http.StatusForbidden)
			return
		}
	} else {
		http.Error(w, `{"error":"org or user context required"}`, http.StatusUnauthorized)
		return
	}

	orgs, err := r.ListInstalls(spaceID)
	if err != nil {
		http.Error(w, `{"error":"list failed"}`, 500)
		return
	}
	if orgs == nil {
		orgs = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"space_id": spaceID, "orgs": orgs})
}

// HandleSetDistribution updates a space's distribution mode. Publisher-only.
// PUT /api/spaces/{spaceId}/distribution  { "distribution": "public" | ... }
func (r *Registry) HandleSetDistribution(w http.ResponseWriter, req *http.Request) {
	orgID := req.Header.Get("X-Auth-Org-ID")
	userID := req.Header.Get("X-Auth-User-ID")
	spaceID := req.PathValue("spaceId")

	if orgID != "" {
		publisher := r.GetSpacePublisherOrg(spaceID)
		if publisher == "" || publisher != orgID {
			http.Error(w, `{"error":"forbidden: not the publisher org"}`, http.StatusForbidden)
			return
		}
	} else if userID != "" {
		owner := r.GetSpaceOwnerUser(spaceID)
		if owner == "" || owner != userID {
			http.Error(w, `{"error":"forbidden: not the publisher user"}`, http.StatusForbidden)
			return
		}
	} else {
		http.Error(w, `{"error":"org or user context required"}`, http.StatusUnauthorized)
		return
	}

	var body struct {
		Distribution string `json:"distribution"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, 400)
		return
	}
	if err := r.SetSpaceDistribution(spaceID, body.Distribution); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "distribution": body.Distribution})
}

// HandleAddAllowlist / HandleRemoveAllowlist manage space_allowlist entries.
// Publisher-only.
func (r *Registry) HandleAddAllowlist(w http.ResponseWriter, req *http.Request) {
	r.allowlistMutate(w, req, true)
}

func (r *Registry) HandleRemoveAllowlist(w http.ResponseWriter, req *http.Request) {
	r.allowlistMutate(w, req, false)
}

func (r *Registry) allowlistMutate(w http.ResponseWriter, req *http.Request, add bool) {
	orgID := req.Header.Get("X-Auth-Org-ID")
	userID := req.Header.Get("X-Auth-User-ID")
	spaceID := req.PathValue("spaceId")

	if orgID != "" {
		publisher := r.GetSpacePublisherOrg(spaceID)
		if publisher == "" || publisher != orgID {
			http.Error(w, `{"error":"forbidden: not the publisher org"}`, http.StatusForbidden)
			return
		}
	} else if userID != "" {
		owner := r.GetSpaceOwnerUser(spaceID)
		if owner == "" || owner != userID {
			http.Error(w, `{"error":"forbidden: not the publisher user"}`, http.StatusForbidden)
			return
		}
	} else {
		http.Error(w, `{"error":"org or user context required"}`, http.StatusUnauthorized)
		return
	}

	var body struct {
		OrgID string `json:"org_id"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, 400)
		return
	}
	if body.OrgID == "" {
		http.Error(w, `{"error":"org_id required"}`, 400)
		return
	}
	var err error
	if add {
		err = r.AddToAllowlist(spaceID, body.OrgID)
	} else {
		err = r.RemoveFromAllowlist(spaceID, body.OrgID)
	}
	if err != nil {
		http.Error(w, `{"error":"allowlist mutation failed"}`, 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}
