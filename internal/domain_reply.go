package internal

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
)

const (
	DomainReplyScopeCampaign  = "campaign"
	DomainReplyScopeWorkspace = "workspace"
)

// ParseDomainReplyStop parses a --stop-on-domain-reply value.
// "" and "off" disable the domain stop. "campaign" enables it for the
// replying campaign only; "workspace" enables it for every campaign in the
// same workspace. The returned scope is always a valid stored value.
func ParseDomainReplyStop(value string) (enabled bool, scope string, err error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "off", "false", "no", "0":
		return false, DomainReplyScopeCampaign, nil
	case DomainReplyScopeCampaign, "on", "true", "yes", "1":
		return true, DomainReplyScopeCampaign, nil
	case DomainReplyScopeWorkspace:
		return true, DomainReplyScopeWorkspace, nil
	}
	return false, "", fmt.Errorf("invalid stop-on-domain-reply value %q (expected off, campaign, or workspace)", value)
}

// FormatDomainReplyStop renders the stored columns as a flag value.
func FormatDomainReplyStop(enabled bool, scope string) string {
	if !enabled {
		return "off"
	}
	if scope == DomainReplyScopeWorkspace {
		return DomainReplyScopeWorkspace
	}
	return DomainReplyScopeCampaign
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// applyDomainReplyStop skips pending sends and pauses leads that share the
// replying lead's domain, when the replying campaign has stop_on_domain_reply
// enabled. With domain_reply_scope = 'campaign' only that campaign is touched.
// With 'workspace' every campaign in the same workspace is touched, so a reply
// to one wave of a multi-wave outreach also stops later waves at that firm.
func applyDomainReplyStop(db *sql.DB, campaignID, leadID int64) {
	var stopOnDomainReply int
	var scope, workspaceID string
	if err := queryRowDB(db, "SELECT stop_on_domain_reply, domain_reply_scope, workspace_id FROM campaigns WHERE id = ?", campaignID).
		Scan(&stopOnDomainReply, &scope, &workspaceID); err != nil {
		slog.Warn("failed to load domain reply settings", "campaign_id", campaignID, "error", err)
		return
	}
	if stopOnDomainReply == 0 {
		return
	}

	var domain string
	queryRowDB(db, "SELECT domain FROM leads WHERE id = ?", leadID).Scan(&domain)
	if domain == "" {
		return
	}

	// Both scheduled_sends and campaign_leads carry campaign_id and lead_id,
	// so one WHERE clause serves both updates.
	var where string
	var args []any
	if scope == DomainReplyScopeWorkspace {
		where = `campaign_id IN (SELECT id FROM campaigns WHERE workspace_id = ?)
			AND lead_id IN (SELECT id FROM leads WHERE domain = ?)`
		args = []any{workspaceID, domain}
	} else {
		where = `campaign_id = ?
			AND lead_id IN (SELECT id FROM leads WHERE domain = ? AND id != ?)`
		args = []any{campaignID, domain, leadID}
	}

	var skipped int64
	res, err := execDB(db, "UPDATE scheduled_sends SET status = 'skipped' WHERE "+where+" AND status = 'pending'", args...)
	if err != nil {
		slog.Warn("failed to skip domain sends",
			"campaign_id", campaignID, "domain", domain, "scope", scope, "error", err)
	} else {
		skipped, _ = res.RowsAffected()
	}

	if _, err := execDB(db, "UPDATE campaign_leads SET status = 'paused' WHERE "+where+" AND status = 'active'", args...); err != nil {
		slog.Warn("failed to pause domain leads",
			"campaign_id", campaignID, "domain", domain, "scope", scope, "error", err)
	}

	if skipped > 0 {
		slog.Info("domain reply stop applied",
			"campaign_id", campaignID, "domain", domain, "scope", scope, "sends_skipped", skipped)
	}
}
