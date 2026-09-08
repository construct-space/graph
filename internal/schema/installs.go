package schema

import (
	"fmt"
	"strings"
)

// Distribution controls who may install a space.
const (
	DistributionPublic       = "public"        // marketplace-visible, anyone installs
	DistributionOrgAllowlist = "org_allowlist" // only orgs in space_allowlist
	DistributionPrivate      = "private"       // only publisher_org_id
)

// GetSpaceDistribution returns the space's distribution mode. Defaults to
// "public" when the column is empty (SQLite nullable ALTER TABLE path).
func (r *Registry) GetSpaceDistribution(spaceID string) string {
	var dist string
	r.db.Raw(
		fmt.Sprintf("SELECT COALESCE(distribution, '') FROM %s WHERE id = ?", systemTable("spaces")),
		spaceID,
	).Scan(&dist)
	if dist == "" {
		return DistributionPublic
	}
	return dist
}

// SetSpaceDistribution changes the distribution mode. Caller must verify they
// are authorized (publisher org) — this function does not check.
func (r *Registry) SetSpaceDistribution(spaceID, distribution string) error {
	distribution = strings.TrimSpace(distribution)
	switch distribution {
	case DistributionPublic, DistributionOrgAllowlist, DistributionPrivate:
	default:
		return fmt.Errorf("invalid distribution %q", distribution)
	}
	return r.db.Exec(
		fmt.Sprintf("UPDATE %s SET distribution = ? WHERE id = ?", systemTable("spaces")),
		distribution, spaceID,
	).Error
}

// CanInstall checks whether orgID is allowed to install spaceID under the
// current distribution rules. Returns (allowed, reason). The publisher org is
// always allowed to install its own spaces — even private ones.
func (r *Registry) CanInstall(spaceID, orgID string) (bool, string) {
	if orgID == "" {
		return false, "org context required"
	}
	publisher := r.GetSpacePublisherOrg(spaceID)
	if publisher != "" && publisher == orgID {
		return true, ""
	}
	switch r.GetSpaceDistribution(spaceID) {
	case DistributionPublic:
		return true, ""
	case DistributionOrgAllowlist:
		if r.isAllowlisted(spaceID, orgID) {
			return true, ""
		}
		return false, "org not on allowlist"
	case DistributionPrivate:
		return false, "space is private to its publisher"
	}
	return false, "unknown distribution"
}

// InstallSpace records an install after CanInstall approves. Idempotent — a
// duplicate install is a no-op so the HTTP layer can safely retry.
func (r *Registry) InstallSpace(spaceID, orgID string) error {
	if ok, reason := r.CanInstall(spaceID, orgID); !ok {
		return fmt.Errorf("install denied: %s", reason)
	}
	// ON CONFLICT DO NOTHING via upsert helper — no update cols needed.
	return r.db.Exec(
		fmt.Sprintf(
			"INSERT INTO %s (space_id, org_id) VALUES (?, ?) ON CONFLICT (space_id, org_id) DO NOTHING",
			systemTable("space_installs"),
		),
		spaceID, orgID,
	).Error
}

// UninstallSpace removes the install row. Does not drop tenant data — the
// tenant's schema and rows survive uninstall so data can be recovered if the
// space is later reinstalled.
func (r *Registry) UninstallSpace(spaceID, orgID string) error {
	return r.db.Exec(
		fmt.Sprintf("DELETE FROM %s WHERE space_id = ? AND org_id = ?", systemTable("space_installs")),
		spaceID, orgID,
	).Error
}

// IsInstalled reports whether orgID has installed spaceID. The publisher org
// is treated as installed implicitly (it owns the space).
func (r *Registry) IsInstalled(spaceID, orgID string) bool {
	if orgID == "" {
		return false
	}
	if publisher := r.GetSpacePublisherOrg(spaceID); publisher != "" && publisher == orgID {
		return true
	}
	var count int
	r.db.Raw(
		fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE space_id = ? AND org_id = ?", systemTable("space_installs")),
		spaceID, orgID,
	).Scan(&count)
	return count > 0
}

// ListInstalls returns the org_ids that have installed spaceID, newest first.
// Intended for publisher-admin resolvers.
func (r *Registry) ListInstalls(spaceID string) ([]string, error) {
	rows, err := r.db.Raw(
		fmt.Sprintf("SELECT org_id FROM %s WHERE space_id = ? ORDER BY installed_at DESC", systemTable("space_installs")),
		spaceID,
	).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

// AddToAllowlist grants orgID permission to install a space with
// distribution=org_allowlist. Idempotent.
func (r *Registry) AddToAllowlist(spaceID, orgID string) error {
	return r.db.Exec(
		fmt.Sprintf(
			"INSERT INTO %s (space_id, org_id) VALUES (?, ?) ON CONFLICT (space_id, org_id) DO NOTHING",
			systemTable("space_allowlist"),
		),
		spaceID, orgID,
	).Error
}

// RemoveFromAllowlist revokes an allowlist entry. Existing installs are NOT
// automatically removed — caller may also want to call UninstallSpace.
func (r *Registry) RemoveFromAllowlist(spaceID, orgID string) error {
	return r.db.Exec(
		fmt.Sprintf("DELETE FROM %s WHERE space_id = ? AND org_id = ?", systemTable("space_allowlist")),
		spaceID, orgID,
	).Error
}

func (r *Registry) isAllowlisted(spaceID, orgID string) bool {
	var count int
	r.db.Raw(
		fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE space_id = ? AND org_id = ?", systemTable("space_allowlist")),
		spaceID, orgID,
	).Scan(&count)
	return count > 0
}
