package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/forgelinehq/forgeline/internal/docstore"
)

// Several AWS accounts (note 017). The connection made on the Infrastructure page is the primary
// account and the only one changes are applied to. Added accounts are read-only: a profile and
// region from the server's AWS configuration, shown in the explorer, spend and the assistant.

const (
	kindAWSAccount = "awsaccount"
	primaryAccount = "primary"
	maxAWSAccounts = 50
	accountNameMax = 40
)

var accountEnvs = map[string]bool{"": true, "production": true, "staging": true, "development": true, "sandbox": true}

var errNoAccount = errors.New("no such AWS account")

// AWSAccount is one added account.
type AWSAccount struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Profile   string    `json:"profile"`
	Region    string    `json:"region"`
	Env       string    `json:"env,omitempty"`
	AccountID string    `json:"account_id"`       // proven with sts when it was added
	Writes    bool      `json:"writes,omitempty"` // an administrator allows change proposals for this account
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
}

// AWSAccountJSON is an account as the console and the assistant see it.
type AWSAccountJSON struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Profile   string `json:"profile"`
	Region    string `json:"region"`
	Env       string `json:"env,omitempty"`
	AccountID string `json:"account_id,omitempty"`
	Primary   bool   `json:"primary,omitempty"`
	Writes    bool   `json:"writes"` // changes can be proposed for it (always true for the primary)
}

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

func accountSlug(name string) string {
	s := strings.Trim(slugRE.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if s == "" || s == primaryAccount {
		s = "account"
	}
	if len(s) > 32 {
		s = strings.Trim(s[:32], "-")
	}
	return s
}

// awsFor returns the connection settings for an account: the primary one when id is empty or
// "primary", otherwise an added account. The result may have Enabled false (primary not connected).
func (s *Server) awsFor(ctx context.Context, id string) (awsSettings, error) {
	if id == "" || id == primaryAccount {
		return s.loadAWS(ctx), nil
	}
	var a AWSAccount
	if err := s.docs.Get(ctx, kindAWSAccount, id, &a); errors.Is(err, docstore.ErrNotFound) {
		return awsSettings{}, errNoAccount
	} else if err != nil {
		return awsSettings{}, errors.New("could not read the account")
	}
	return awsSettings{Enabled: true, Profile: a.Profile, Region: a.Region}, nil
}

func (s *Server) listAWSAccounts(ctx context.Context) ([]AWSAccount, error) {
	docs, err := s.docs.List(ctx, kindAWSAccount, docstore.ListOptions{})
	if err != nil {
		return nil, err
	}
	as, err := docstore.Decode[AWSAccount](docs)
	if err != nil {
		return nil, err
	}
	sort.Slice(as, func(i, j int) bool { return strings.ToLower(as[i].Name) < strings.ToLower(as[j].Name) })
	return as, nil
}

// accountViews is every account the console can show: the primary (when connected) first.
func (s *Server) accountViews(ctx context.Context) ([]AWSAccountJSON, error) {
	var out []AWSAccountJSON
	if st := s.loadAWS(ctx); st.Enabled {
		name := "Primary"
		out = append(out, AWSAccountJSON{ID: primaryAccount, Name: name, Profile: st.Profile, Region: st.Region, Primary: true, Writes: true})
	}
	as, err := s.listAWSAccounts(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range as {
		out = append(out, AWSAccountJSON{ID: a.ID, Name: a.Name, Profile: a.Profile, Region: a.Region, Env: a.Env, AccountID: a.AccountID, Writes: a.Writes})
	}
	if out == nil {
		out = []AWSAccountJSON{}
	}
	return out, nil
}

func (s *Server) handleListAWSAccounts(w http.ResponseWriter, r *http.Request) {
	v, err := s.accountViews(r.Context())
	if err != nil {
		s.internalError(w, "reading the accounts", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": v})
}

type addAccountRequest struct {
	Name    string `json:"name"`
	Profile string `json:"profile"`
	Region  string `json:"region"`
	Env     string `json:"env"`
}

func (s *Server) handleAddAWSAccount(w http.ResponseWriter, r *http.Request) {
	var req addAccountRequest
	if !readJSON(w, r, &req) {
		return
	}
	req.Name, req.Profile, req.Region = strings.TrimSpace(req.Name), strings.TrimSpace(req.Profile), strings.TrimSpace(req.Region)
	switch {
	case req.Name == "" || len(req.Name) > accountNameMax:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("give the account a name of up to %d characters", accountNameMax))
		return
	case req.Profile == "" || !awsProfileRE.MatchString(req.Profile):
		writeError(w, http.StatusBadRequest, "the profile name may contain only letters, digits and . _ @ -")
		return
	case req.Profile == ssoProfile:
		writeError(w, http.StatusBadRequest, "that profile name is reserved for signing in from the console")
		return
	case !awsRegionRE.MatchString(req.Region):
		writeError(w, http.StatusBadRequest, "the region must look like ap-south-1")
		return
	case !accountEnvs[req.Env]:
		writeError(w, http.StatusBadRequest, "the environment must be production, staging, development or sandbox")
		return
	}
	ctx := r.Context()
	existing, err := s.listAWSAccounts(ctx)
	if err != nil {
		s.internalError(w, "reading the accounts", err)
		return
	}
	if len(existing) >= maxAWSAccounts {
		writeError(w, http.StatusConflict, fmt.Sprintf("at most %d accounts can be added", maxAWSAccounts))
		return
	}

	st := awsSettings{Enabled: true, Profile: req.Profile, Region: req.Region}
	out, err := s.aws(ctx, st, "sts", "get-caller-identity")
	if err != nil {
		s.auditReq(r, "integration.aws.account.add", "integration/aws/"+req.Name, "profile "+req.Profile+" refused: "+err.Error(), false)
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	var id struct{ Account string }
	_ = json.Unmarshal(out, &id)
	if id.Account == "" {
		writeError(w, http.StatusUnprocessableEntity, "AWS did not say which account that profile is")
		return
	}
	// The same account twice would only double the reads, and two names for it would confuse an approver.
	for _, a := range existing {
		if a.AccountID == id.Account {
			writeError(w, http.StatusConflict, fmt.Sprintf("account %s is already added as %q", id.Account, a.Name))
			return
		}
		if strings.EqualFold(a.Name, req.Name) {
			writeError(w, http.StatusConflict, fmt.Sprintf("there is already an account named %q", a.Name))
			return
		}
	}
	if prim := s.loadAWS(ctx); prim.Enabled {
		if pout, perr := s.aws(ctx, prim, "sts", "get-caller-identity"); perr == nil {
			var pid struct{ Account string }
			_ = json.Unmarshal(pout, &pid)
			if pid.Account == id.Account {
				writeError(w, http.StatusConflict, fmt.Sprintf("account %s is the primary connection already", id.Account))
				return
			}
		}
	}

	p := principalFrom(ctx)
	a := AWSAccount{ID: accountSlug(req.Name), Name: req.Name, Profile: req.Profile, Region: req.Region, Env: req.Env,
		AccountID: id.Account, CreatedAt: time.Now().UTC(), CreatedBy: p.Name}
	for taken := true; taken; { // keep the slug unique without telling the person about it
		var tmp AWSAccount
		if err := s.docs.Get(ctx, kindAWSAccount, a.ID, &tmp); errors.Is(err, docstore.ErrNotFound) {
			taken = false
		} else if err != nil {
			s.internalError(w, "saving the account", err)
			return
		} else {
			a.ID = accountSlug(req.Name) + "-" + randomID()[:4]
		}
	}
	if err := s.docs.Put(ctx, kindAWSAccount, a.ID, a); err != nil {
		s.internalError(w, "saving the account", err)
		return
	}
	s.auditReq(r, "integration.aws.account.add", "integration/aws/"+a.ID, fmt.Sprintf("%s: profile %q, region %s, account %s", a.Name, a.Profile, a.Region, a.AccountID), true)
	writeJSON(w, http.StatusCreated, AWSAccountJSON{ID: a.ID, Name: a.Name, Profile: a.Profile, Region: a.Region, Env: a.Env, AccountID: a.AccountID})
}

func (s *Server) handleDeleteAWSAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.acctMu.Lock()
	defer s.acctMu.Unlock()
	if id == primaryAccount {
		writeError(w, http.StatusBadRequest, "the primary connection is changed on the Infrastructure page")
		return
	}
	var a AWSAccount
	if err := s.docs.Get(r.Context(), kindAWSAccount, id, &a); errors.Is(err, docstore.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such account")
		return
	} else if err != nil {
		s.internalError(w, "reading the account", err)
		return
	}
	if err := s.docs.Delete(r.Context(), kindAWSAccount, id); err != nil {
		s.internalError(w, "removing the account", err)
		return
	}
	n, cerr := s.cancelAccountProposals(r.Context(), id, a.Name, "the account was removed")
	s.auditReq(r, "integration.aws.account.remove", "integration/aws/"+id, fmt.Sprintf("%s (%d waiting proposals canceled)%s", a.Name, n, map[bool]string{true: ", some could not be", false: ""}[cerr != nil]), cerr == nil)
	if cerr != nil {
		s.internalError(w, "canceling the proposals waiting for the account (it was removed; cancel them by hand)", cerr)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type updateAccountRequest struct {
	Writes *bool `json:"writes"`
}

// handleUpdateAWSAccount switches change proposals on or off for an added account. Only an
// administrator can, so nobody can widen where changes may go by proposing them.
func (s *Server) handleUpdateAWSAccount(w http.ResponseWriter, r *http.Request) {
	var req updateAccountRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Writes == nil {
		writeError(w, http.StatusBadRequest, "say writes: true or false")
		return
	}
	ctx := r.Context()
	id := r.PathValue("id")
	s.acctMu.Lock()
	defer s.acctMu.Unlock()
	if id == primaryAccount {
		writeError(w, http.StatusBadRequest, "changes can always be proposed for the primary connection")
		return
	}
	var a AWSAccount
	if err := s.docs.Get(ctx, kindAWSAccount, id, &a); errors.Is(err, docstore.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such account")
		return
	} else if err != nil {
		s.internalError(w, "reading the account", err)
		return
	}
	a.Writes = *req.Writes
	if err := s.docs.Put(ctx, kindAWSAccount, id, a); err != nil {
		s.internalError(w, "saving the account", err)
		return
	}
	detail := fmt.Sprintf("%s: changes %s", a.Name, map[bool]string{true: "allowed", false: "not allowed"}[a.Writes])
	var cerr error
	if !a.Writes {
		var n int
		n, cerr = s.cancelAccountProposals(ctx, id, a.Name, "changes were turned off for the account")
		detail += fmt.Sprintf(" (%d waiting proposals canceled)", n)
	}
	s.auditReq(r, "integration.aws.account.writes", "integration/aws/"+id, detail, cerr == nil)
	if cerr != nil {
		s.internalError(w, "canceling the proposals waiting for the account (changes are off; cancel them by hand)", cerr)
		return
	}
	writeJSON(w, http.StatusOK, AWSAccountJSON{ID: a.ID, Name: a.Name, Profile: a.Profile, Region: a.Region, Env: a.Env, AccountID: a.AccountID, Writes: a.Writes})
}

// writeTarget resolves the account a change would be applied to and the string that binds a
// proposal to it. The primary connection keeps the plain "profile/region" form; an added account
// must have changes allowed and its target also names its AWS account number.
func (s *Server) writeTarget(ctx context.Context, id string) (st awsSettings, acct AWSAccount, target string, err error) {
	if id == "" || id == primaryAccount {
		st = s.loadAWS(ctx)
		return st, AWSAccount{}, fmt.Sprintf("%s/%s", st.Profile, st.Region), nil
	}
	if err = s.docs.Get(ctx, kindAWSAccount, id, &acct); errors.Is(err, docstore.ErrNotFound) {
		return st, acct, "", errNoAccount
	} else if err != nil {
		return st, acct, "", errors.New("could not read the account")
	}
	if !acct.Writes {
		return st, acct, "", fmt.Errorf("an administrator has not allowed changes for %q (Infrastructure → AWS account → Allow changes)", acct.Name)
	}
	st = awsSettings{Enabled: true, Profile: acct.Profile, Region: acct.Region}
	return st, acct, fmt.Sprintf("acct/%s/%s/%s/%s", acct.ID, acct.AccountID, acct.Profile, acct.Region), nil
}

// verifyAccountIdentity checks that an added account's profile still signs in to the AWS account
// it was added as. It returns "" when it does, otherwise what to tell the person.
func (s *Server) verifyAccountIdentity(ctx context.Context, st awsSettings, acct AWSAccount) (problem string, unreachable error) {
	out, err := s.aws(ctx, st, "sts", "get-caller-identity")
	if err != nil {
		return "", err
	}
	var ident struct{ Account string }
	_ = json.Unmarshal(out, &ident)
	if ident.Account != acct.AccountID {
		return fmt.Sprintf("the profile %q now points at AWS account %s, not %s", acct.Profile, ident.Account, acct.AccountID), nil
	}
	return "", nil
}

// cancelAccountProposals cancels every proposal still waiting for an account. A change that is
// already running is left to finish; it can no longer be started. It reports how many it canceled
// and whether anything could not be read or saved, because an administrator who is told "none" must
// be able to trust it.
func (s *Server) cancelAccountProposals(ctx context.Context, id, name, why string) (n int, err error) {
	now := time.Now().UTC()
	note := "canceled automatically: " + why
	var cleanup []CloudChange
	s.cloud.mu.Lock()
	docs, lerr := s.docs.List(ctx, kindCloud, docstore.ListOptions{})
	if lerr != nil {
		err = lerr
	}
	cs, _ := docstore.Decode[CloudChange](docs)
	for _, c := range cs {
		if c.Conn != id || (c.Status != stProposed && c.Status != stPreparing) {
			continue
		}
		c.Status, c.FinishedAt, c.DecisionNote = stCanceled, &now, note
		if perr := s.docs.Put(ctx, kindCloud, c.ID, c); perr != nil {
			err = perr
			continue
		}
		n++
		if c.Kind == kindStackDeploy {
			cleanup = append(cleanup, c)
		}
	}
	s.cloud.mu.Unlock()
	for i := range cleanup {
		s.cleanupChangeSet(ctx, awsSettings{Profile: cleanup[i].Profile, Region: cleanup[i].Region}, &cleanup[i])
	}
	s.aiMu.Lock()
	defer s.aiMu.Unlock()
	docs, lerr = s.docs.List(ctx, kindAIProposal, docstore.ListOptions{})
	if lerr != nil {
		err = lerr
	}
	ps, _ := docstore.Decode[AIProposal](docs)
	for _, p := range ps {
		if p.Account != id || p.Status != apProposed {
			continue
		}
		p.Status, p.FinishedAt, p.Note = apCancelled, &now, note
		if perr := s.docs.Put(ctx, kindAIProposal, p.ID, p); perr != nil {
			err = perr
			continue
		}
		n++
	}
	return n, err
}

// accountParam reads ?account= and resolves it. It answers the request itself when the account is
// unknown, and returns ok=false.
func (s *Server) accountParam(w http.ResponseWriter, r *http.Request) (awsSettings, string, bool) {
	id := r.URL.Query().Get("account")
	if id == "" {
		id = primaryAccount
	}
	st, err := s.awsFor(r.Context(), id)
	if errors.Is(err, errNoAccount) {
		writeError(w, http.StatusNotFound, "no such account")
		return st, id, false
	} else if err != nil {
		s.internalError(w, "reading the account", err)
		return st, id, false
	}
	return st, id, true
}

// lookupAWSAccounts lists the accounts the assistant may ask about.
func (s *Server) lookupAWSAccounts(ctx context.Context, _ json.RawMessage) (string, error) {
	v, err := s.accountViews(ctx)
	if err != nil {
		return "", errors.New("could not read the accounts")
	}
	if len(v) == 0 {
		return "No AWS account is connected.", nil
	}
	var b strings.Builder
	for _, a := range v {
		fmt.Fprintf(&b, "- id %q: %s", a.ID, a.Name)
		if a.AccountID != "" {
			fmt.Fprintf(&b, ", account %s", a.AccountID)
		}
		fmt.Fprintf(&b, ", region %s", a.Region)
		if a.Env != "" {
			fmt.Fprintf(&b, ", %s", a.Env)
		}
		if a.Primary {
			b.WriteString(" (primary: changes can be proposed for it)")
		} else if a.Writes {
			b.WriteString(" (changes can be proposed for it: pass this id as account)")
		} else {
			b.WriteString(" (read-only: an administrator has not allowed changes)")
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

// hasAddedAccounts says whether call_aws has anything to reach while the MCP server owns the primary.
func (s *Server) hasAddedAccounts(ctx context.Context) bool {
	as, err := s.listAWSAccounts(ctx)
	return err == nil && len(as) > 0
}
