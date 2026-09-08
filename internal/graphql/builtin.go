package graphql

import (
	"context"
	"fmt"
)

// resolveBuiltin handles platform-level queries that don't correspond to a
// model in any space's manifest. Today the only built-in is `_installs`,
// which exposes publisher-admin visibility into which orgs installed a space.
//
// Gate: every built-in requires rctx.orgID to equal the current space's
// publisher_org_id. Legacy spaces with no publisher_org_id cannot reach these.
func (h *Handler) resolveBuiltin(_ context.Context, rctx *requestContext, op, args string, vars map[string]any) (map[string]any, error) {
	_ = args // reserved for future arg-string parsing

	publisher := h.registry.GetSpacePublisherOrg(rctx.spaceID)
	if publisher == "" {
		return nil, fmt.Errorf("built-in queries require a publisher_org_id on the space")
	}
	if rctx.orgID == "" {
		return nil, fmt.Errorf("built-in queries require an authenticated org context")
	}
	if rctx.orgID != publisher {
		return nil, fmt.Errorf("built-in queries restricted to the publisher org")
	}

	result := make(map[string]any)
	switch op {
	case "_installs":
		targetSpace := extractString(args, vars, "spaceId")
		if targetSpace == "" {
			// Default: list installs of the space the caller is querying from.
			targetSpace = rctx.spaceID
		}
		// Cross-space visibility is restricted to sibling spaces in the same
		// bundle — admin space can list installs of its kanban sibling, but
		// not of unrelated spaces from other publishers.
		if targetSpace != rctx.spaceID {
			selfBundle := h.registry.GetSpaceBundleIDFor(rctx.spaceID)
			targetBundle := h.registry.GetSpaceBundleIDFor(targetSpace)
			if selfBundle == "" || selfBundle != targetBundle {
				return nil, fmt.Errorf("_installs: target space is not in the same bundle")
			}
		}
		orgs, err := h.registry.ListInstalls(targetSpace)
		if err != nil {
			return nil, fmt.Errorf("_installs: %w", err)
		}
		rows := make([]map[string]any, 0, len(orgs))
		for _, org := range orgs {
			rows = append(rows, map[string]any{"orgId": org, "spaceId": targetSpace})
		}
		result[op] = rows
	default:
		return nil, fmt.Errorf("unknown built-in query: %s", op)
	}
	return result, nil
}
