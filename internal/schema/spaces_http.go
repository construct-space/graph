package schema

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// ListSpacesByOwnerOrg returns every space where publisher_org_id equals the
// given org. Each row carries enough publisher-dashboard detail (bundle,
// distribution, install count) without pulling the full manifest.
func (r *Registry) ListSpacesByOwnerOrg(ownerOrgID string) ([]SpaceSummary, error) {
	rows, err := r.db.Raw(
		fmt.Sprintf(`SELECT id, name, latest_version,
			COALESCE(bundle_id,'') AS bundle_id,
			COALESCE(distribution,'public') AS distribution,
			COALESCE(publisher_org_id,'') AS publisher_org_id
			FROM %s
			WHERE publisher_org_id = ?
			ORDER BY registered_at DESC`, systemTable("spaces")),
		ownerOrgID,
	).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SpaceSummary
	for rows.Next() {
		var s SpaceSummary
		if err := rows.Scan(&s.ID, &s.Name, &s.LatestVersion, &s.BundleID, &s.Distribution, &s.PublisherOrgID); err != nil {
			return nil, err
		}
		out = append(out, s)
	}

	// Populate install counts with one query per space. Count tables stay
	// tiny (one row per tenant install); the alternative is a JOIN + GROUP BY
	// that adds complexity for little gain at publisher scale.
	installsTable := systemTable("space_installs")
	for i := range out {
		var n int
		r.db.Raw(
			fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE space_id = ?", installsTable),
			out[i].ID,
		).Scan(&n)
		out[i].InstallCount = n
	}
	return out, nil
}

// ListAllSpaces returns every space across all orgs/users. Caller is
// responsible for authorization — admin endpoints (oracle) gate this
// with X-Internal-Secret. Used by Oracle's All Spaces page so the
// marketplace-review surface sees every space that has runtime presence
// on graph, not just the subset that opted into developer's lifecycle.
func (r *Registry) ListAllSpaces() ([]SpaceSummary, error) {
	rows, err := r.db.Raw(
		fmt.Sprintf(`SELECT id, name, latest_version,
			COALESCE(bundle_id,'') AS bundle_id,
			COALESCE(distribution,'public') AS distribution,
			COALESCE(publisher_org_id,'') AS publisher_org_id
			FROM %s
			ORDER BY registered_at DESC`, systemTable("spaces")),
	).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SpaceSummary
	for rows.Next() {
		var s SpaceSummary
		if err := rows.Scan(&s.ID, &s.Name, &s.LatestVersion, &s.BundleID, &s.Distribution, &s.PublisherOrgID); err != nil {
			return nil, err
		}
		out = append(out, s)
	}

	installsTable := systemTable("space_installs")
	for i := range out {
		var n int
		r.db.Raw(
			fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE space_id = ?", installsTable),
			out[i].ID,
		).Scan(&n)
		out[i].InstallCount = n
	}
	return out, nil
}

// AdminListSpaces serves GET /api/admin/spaces. Same shape as
// HandleListSpaces but unfiltered by owner — every row in the spaces
// system table. Gated upstream by requireAdminAuth.
func (r *Registry) AdminListSpaces(w http.ResponseWriter, _ *http.Request) {
	spaces, err := r.ListAllSpaces()
	if err != nil {
		http.Error(w, `{"error":"list failed"}`, 500)
		return
	}
	if spaces == nil {
		spaces = []SpaceSummary{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"spaces": spaces, "total": len(spaces)})
}

// ListSpacesByOwnerUser returns every space where owner_user_id equals the
// given user ID.
func (r *Registry) ListSpacesByOwnerUser(ownerUserID string) ([]SpaceSummary, error) {
	rows, err := r.db.Raw(
		fmt.Sprintf(`SELECT id, name, latest_version,
			COALESCE(bundle_id,'') AS bundle_id,
			COALESCE(distribution,'public') AS distribution,
			COALESCE(publisher_org_id,'') AS publisher_org_id
			FROM %s
			WHERE owner_user_id = ?
			ORDER BY registered_at DESC`, systemTable("spaces")),
		ownerUserID,
	).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SpaceSummary
	for rows.Next() {
		var s SpaceSummary
		if err := rows.Scan(&s.ID, &s.Name, &s.LatestVersion, &s.BundleID, &s.Distribution, &s.PublisherOrgID); err != nil {
			return nil, err
		}
		out = append(out, s)
	}

	installsTable := systemTable("space_installs")
	for i := range out {
		var n int
		r.db.Raw(
			fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE space_id = ?", installsTable),
			out[i].ID,
		).Scan(&n)
		out[i].InstallCount = n
	}
	return out, nil
}

// HandleListSpaces returns the caller org's (or user's) spaces.
// GET /api/spaces
func (r *Registry) HandleListSpaces(w http.ResponseWriter, req *http.Request) {
	orgID := req.Header.Get("X-Auth-Org-ID")
	userID := req.Header.Get("X-Auth-User-ID")

	var spaces []SpaceSummary
	var err error

	if orgID != "" {
		spaces, err = r.ListSpacesByOwnerOrg(orgID)
	} else if userID != "" {
		spaces, err = r.ListSpacesByOwnerUser(userID)
	} else {
		http.Error(w, `{"error":"org or user context required"}`, http.StatusUnauthorized)
		return
	}

	if err != nil {
		http.Error(w, `{"error":"list failed"}`, 500)
		return
	}
	if spaces == nil {
		spaces = []SpaceSummary{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"spaces": spaces})
}
