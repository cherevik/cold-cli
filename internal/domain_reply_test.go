package internal

import (
	"database/sql"
	"testing"
)

// setupDomainReplyDB builds: wave1 and wave2 in the default workspace, other-ws in
// workspace "other". john@acme.com (lead 1) is in wave1 with a sent step 1 and a
// pending step 2; jane@acme.com (lead 2) has pending step 1 in wave2 and other-ws;
// bob@other.com (lead 3) has pending step 1 in wave2.
func setupDomainReplyDB(t *testing.T, scope string) *sql.DB {
	t.Helper()
	db := testDB(t)
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec("INSERT INTO accounts (email) VALUES ('sender@x.com')")
	mustExec("INSERT INTO campaigns (workspace_id, name, status, sequence_file, stop_on_domain_reply, domain_reply_scope) VALUES ('default', 'wave1', 'active', 'seq.yml', 1, ?)", scope)
	mustExec("INSERT INTO campaigns (workspace_id, name, status, sequence_file) VALUES ('default', 'wave2', 'active', 'seq.yml')")
	mustExec("INSERT INTO campaigns (workspace_id, name, status, sequence_file) VALUES ('other', 'other-ws', 'active', 'seq.yml')")
	mustExec("INSERT INTO leads (email, first_name, domain) VALUES ('john@acme.com', 'John', 'acme.com')")
	mustExec("INSERT INTO leads (email, first_name, domain) VALUES ('jane@acme.com', 'Jane', 'acme.com')")
	mustExec("INSERT INTO leads (email, first_name, domain) VALUES ('bob@other.com', 'Bob', 'other.com')")
	for _, cl := range [][2]int{{1, 1}, {2, 2}, {2, 3}, {3, 2}} {
		mustExec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (?, ?, 'active')", cl[0], cl[1])
	}
	mustExec("INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (1, 1)")
	mustExec(`INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id)
		VALUES (1, 1, 1, 'sent', 1, '<sent-john@gmail.com>', 'thread-john')`)
	for _, ss := range [][3]int{{1, 1, 2}, {2, 2, 1}, {2, 3, 1}, {3, 2, 1}} {
		mustExec(`INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status)
			VALUES (?, ?, 1, ?, '2099-01-01', 'pending')`, ss[0], ss[1], ss[2])
	}
	return db
}

func sendStatus(t *testing.T, db *sql.DB, campaignID, leadID int) string {
	t.Helper()
	var s string
	if err := db.QueryRow("SELECT status FROM scheduled_sends WHERE campaign_id = ? AND lead_id = ?", campaignID, leadID).Scan(&s); err != nil {
		t.Fatalf("send status campaign %d lead %d: %v", campaignID, leadID, err)
	}
	return s
}

func campaignLeadStatus(t *testing.T, db *sql.DB, campaignID, leadID int) string {
	t.Helper()
	var s string
	if err := db.QueryRow("SELECT status FROM campaign_leads WHERE campaign_id = ? AND lead_id = ?", campaignID, leadID).Scan(&s); err != nil {
		t.Fatalf("campaign_lead status campaign %d lead %d: %v", campaignID, leadID, err)
	}
	return s
}

func replyFromJohn(subject string) *MockGWS {
	return &MockGWS{InboxMessages: []GWSMessage{
		{ID: "reply-john", ThreadID: "thread-john", InReplyTo: "<sent-john@gmail.com>",
			From: "john@acme.com", Subject: subject},
	}}
}

var domainReplyAccounts = []Account{{ID: 1, Email: "sender@x.com", DailyLimit: 50, Status: "active"}}

func TestDomainReplyStop_WorkspaceScopeStopsOtherCampaigns(t *testing.T) {
	db := setupDomainReplyDB(t, DomainReplyScopeWorkspace)

	replies, _, err := ProcessReplies(db, replyFromJohn("Re: hello"), domainReplyAccounts)
	if err != nil {
		t.Fatalf("ProcessReplies: %v", err)
	}
	if replies != 1 {
		t.Fatalf("expected 1 reply, got %d", replies)
	}

	if s := sendStatus(t, db, 1, 1); s != "skipped" {
		t.Errorf("wave1 john step 2: want skipped, got %q", s)
	}
	if s := sendStatus(t, db, 2, 2); s != "skipped" {
		t.Errorf("wave2 jane (same domain, same workspace): want skipped, got %q", s)
	}
	if s := campaignLeadStatus(t, db, 2, 2); s != "paused" {
		t.Errorf("wave2 jane campaign_lead: want paused, got %q", s)
	}
	if s := sendStatus(t, db, 2, 3); s != "pending" {
		t.Errorf("wave2 bob (other domain): want pending, got %q", s)
	}
	if s := sendStatus(t, db, 3, 2); s != "pending" {
		t.Errorf("other-ws jane (other workspace): want pending, got %q", s)
	}
}

func TestDomainReplyStop_CampaignScopeLeavesOtherCampaigns(t *testing.T) {
	db := setupDomainReplyDB(t, DomainReplyScopeCampaign)

	if _, _, err := ProcessReplies(db, replyFromJohn("Re: hello"), domainReplyAccounts); err != nil {
		t.Fatalf("ProcessReplies: %v", err)
	}

	if s := sendStatus(t, db, 1, 1); s != "skipped" {
		t.Errorf("wave1 john step 2: want skipped, got %q", s)
	}
	if s := sendStatus(t, db, 2, 2); s != "pending" {
		t.Errorf("wave2 jane with campaign scope: want pending, got %q", s)
	}
	if s := campaignLeadStatus(t, db, 2, 2); s != "active" {
		t.Errorf("wave2 jane campaign_lead: want active, got %q", s)
	}
}

func TestDomainReplyStop_DisabledDoesNothing(t *testing.T) {
	db := setupDomainReplyDB(t, DomainReplyScopeWorkspace)
	db.Exec("UPDATE campaigns SET stop_on_domain_reply = 0 WHERE id = 1")

	if _, _, err := ProcessReplies(db, replyFromJohn("Re: hello"), domainReplyAccounts); err != nil {
		t.Fatalf("ProcessReplies: %v", err)
	}
	if s := sendStatus(t, db, 2, 2); s != "pending" {
		t.Errorf("wave2 jane with domain stop off: want pending, got %q", s)
	}
}

func TestDomainReplyStop_UnsubscribeAppliesDomainStop(t *testing.T) {
	db := setupDomainReplyDB(t, DomainReplyScopeWorkspace)

	_, unsubs, err := ProcessReplies(db, replyFromJohn("Please remove me"), domainReplyAccounts)
	if err != nil {
		t.Fatalf("ProcessReplies: %v", err)
	}
	if unsubs != 1 {
		t.Fatalf("expected 1 unsubscribe, got %d", unsubs)
	}

	var global string
	db.QueryRow("SELECT global_status FROM leads WHERE id = 1").Scan(&global)
	if global != "blacklisted" {
		t.Errorf("john global_status: want blacklisted, got %q", global)
	}
	if s := sendStatus(t, db, 1, 1); s != "cancelled" {
		t.Errorf("wave1 john step 2 after unsubscribe: want cancelled, got %q", s)
	}
	if s := sendStatus(t, db, 2, 2); s != "skipped" {
		t.Errorf("wave2 jane after colleague unsubscribed: want skipped, got %q", s)
	}
	if s := sendStatus(t, db, 2, 3); s != "pending" {
		t.Errorf("wave2 bob: want pending, got %q", s)
	}
}

func TestParseDomainReplyStop(t *testing.T) {
	cases := []struct {
		in      string
		enabled bool
		scope   string
		wantErr bool
	}{
		{"", false, DomainReplyScopeCampaign, false},
		{"off", false, DomainReplyScopeCampaign, false},
		{"campaign", true, DomainReplyScopeCampaign, false},
		{"on", true, DomainReplyScopeCampaign, false},
		{" Workspace ", true, DomainReplyScopeWorkspace, false},
		{"everywhere", false, "", true},
	}
	for _, c := range cases {
		enabled, scope, err := ParseDomainReplyStop(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("%q: err=%v, wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if err == nil && (enabled != c.enabled || scope != c.scope) {
			t.Errorf("%q: got (%v, %q), want (%v, %q)", c.in, enabled, scope, c.enabled, c.scope)
		}
	}
	if got := FormatDomainReplyStop(false, DomainReplyScopeWorkspace); got != "off" {
		t.Errorf("FormatDomainReplyStop disabled: got %q", got)
	}
	if got := FormatDomainReplyStop(true, DomainReplyScopeWorkspace); got != "workspace" {
		t.Errorf("FormatDomainReplyStop workspace: got %q", got)
	}
}

func TestCreateAndUpdateCampaign_StopOnDomainReply(t *testing.T) {
	db := testDB(t)
	t.Setenv("COLD_CLI_DATA_DIR", t.TempDir())
	if _, err := db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 50)"); err != nil {
		t.Fatalf("inserting account: %v", err)
	}
	seqInline := "name: Test\nsteps:\n  - step: 1\n    delay: 0\n    subject: \"Hi {{first_name}}\"\n    body: \"Hello {{first_name}}\"\n"
	leadsInline := "email,first_name\nalice@example.com,Alice\n"

	if _, err := CreateCampaign(db, CreateCampaignOpts{
		Name: "bad", SequenceInline: seqInline, LeadsInline: leadsInline,
		AccountEmails: []string{"sender@x.com"}, StopOnDomainReply: "everywhere",
	}); err == nil {
		t.Fatalf("expected error for invalid stop-on-domain-reply value")
	}

	if _, err := CreateCampaign(db, CreateCampaignOpts{
		Name: "waves", SequenceInline: seqInline, LeadsInline: leadsInline,
		AccountEmails: []string{"sender@x.com"}, StopOnDomainReply: "workspace",
	}); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	var stop int
	var scope string
	db.QueryRow("SELECT stop_on_domain_reply, domain_reply_scope FROM campaigns WHERE name = 'waves'").Scan(&stop, &scope)
	if stop != 1 || scope != DomainReplyScopeWorkspace {
		t.Fatalf("after create: got (%d, %q)", stop, scope)
	}

	info, err := GetCampaignStatus(db, "waves")
	if err != nil {
		t.Fatalf("GetCampaignStatus: %v", err)
	}
	if info.StopOnDomainReply != "workspace" {
		t.Errorf("status StopOnDomainReply: got %q", info.StopOnDomainReply)
	}

	off := "off"
	if err := UpdateCampaign(db, "waves", UpdateCampaignOpts{StopOnDomainReply: &off}); err != nil {
		t.Fatalf("UpdateCampaign off: %v", err)
	}
	db.QueryRow("SELECT stop_on_domain_reply, domain_reply_scope FROM campaigns WHERE name = 'waves'").Scan(&stop, &scope)
	if stop != 0 {
		t.Errorf("after update off: stop=%d", stop)
	}

	campaign := "campaign"
	if err := UpdateCampaign(db, "waves", UpdateCampaignOpts{StopOnDomainReply: &campaign}); err != nil {
		t.Fatalf("UpdateCampaign campaign: %v", err)
	}
	db.QueryRow("SELECT stop_on_domain_reply, domain_reply_scope FROM campaigns WHERE name = 'waves'").Scan(&stop, &scope)
	if stop != 1 || scope != DomainReplyScopeCampaign {
		t.Errorf("after update campaign: got (%d, %q)", stop, scope)
	}

	bad := "nope"
	if err := UpdateCampaign(db, "waves", UpdateCampaignOpts{StopOnDomainReply: &bad}); err == nil {
		t.Errorf("expected error for invalid update value")
	}
}
