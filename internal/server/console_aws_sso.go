package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/forgelinehq/forgeline/internal/docstore"
)

// Signing in to AWS from the console: the IAM Identity Center (SSO) device flow. The person enters
// the SSO start address, is shown a code and a link, approves in their own browser, and picks an
// account and role. Forgeline never sees a password. It keeps the SSO access token encrypted in the
// vault, exchanges it for short-lived role credentials when it needs them, and hands those only to
// the aws command it runs. See design note 009.

const (
	// ssoProfile is the reserved awsSettings.Profile value that means "use the console's SSO
	// sign-in". A real profile with this name cannot be configured.
	ssoProfile = "forgeline-sso"

	idAWSSSO         = "aws-sso"
	ssoTokenName     = "forgeline.aws.sso"
	ssoClientName    = "forgeline"
	ssoScope         = "sso:account:access"
	ssoLoginTTL      = 15 * time.Minute
	ssoCredsMargin   = 5 * time.Minute
	ssoMaxBody       = 1 << 20
	ssoDefaultSlow   = 5 * time.Second
	ssoDeviceGrant   = "urn:ietf:params:oauth:grant-type:device_code"
	ssoStartURLMax   = 300
	ssoMaxLoginsOpen = 8
)

// ssoConfig is which account and role the console is signed in to.
type ssoConfig struct {
	StartURL    string    `json:"start_url"`
	SSORegion   string    `json:"sso_region"`
	AccountID   string    `json:"account_id,omitempty"`
	AccountName string    `json:"account_name,omitempty"`
	RoleName    string    `json:"role_name,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
	UpdatedBy   string    `json:"updated_by"`
}

// ssoToken is what the vault holds: the access token from a completed sign-in.
type ssoToken struct {
	AccessToken string    `json:"access_token"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// ssoLogin is a sign-in in progress, kept in memory only.
type ssoLogin struct {
	clientID, clientSecret, deviceCode string
	startURL, region                   string
	expires                            time.Time
	interval                           time.Duration
	lastPoll                           time.Time
	by                                 string
}

type ssoState struct {
	mu     sync.Mutex
	logins map[string]*ssoLogin
	creds  *ssoCreds
}

type ssoCreds struct {
	env     []string // AWS_ACCESS_KEY_ID=... and so on
	expires time.Time
	key     string // account/role the credentials are for
}

// --- talking to AWS ---

func (s *Server) ssoURLs(region string) (oidc, portal string) {
	if b := s.cfg.SSOBase; b != "" { // a stand-in for development and tests
		return b + "/oidc", b + "/portal"
	}
	return "https://oidc." + region + ".amazonaws.com", "https://portal.sso." + region + ".amazonaws.com"
}

// ssoCall makes one JSON call and returns the status and, for errors, AWS's error code.
func (s *Server) ssoCall(ctx context.Context, method, rawURL string, headers map[string]string, in, out any) (status int, errCode string, err error) {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("accept", "application/json")
	if in != nil {
		req.Header.Set("content-type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", errors.New("could not reach AWS sign-in")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, ssoMaxBody))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		json.Unmarshal(raw, &e)
		code := e.Error
		if code == "" {
			code = resp.Header.Get("x-amzn-ErrorType")
		}
		if i := strings.IndexAny(code, ":#"); i >= 0 { // "Type:http://..." and "ns#Type" forms
			code = strings.TrimLeft(code[:i], " ")
		}
		return resp.StatusCode, code, fmt.Errorf("AWS answered %d %s", resp.StatusCode, truncate(firstLine(e.Message+" "+code), 160))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, "", errors.New("AWS sent a reply that could not be read")
		}
	}
	return resp.StatusCode, "", nil
}

// --- storage ---

func (s *Server) loadSSO(ctx context.Context) (ssoConfig, bool) {
	var c ssoConfig
	if err := s.docs.Get(ctx, kindSettings, idAWSSSO, &c); err != nil {
		return ssoConfig{}, false
	}
	return c, true
}

func (s *Server) loadSSOToken(ctx context.Context) (ssoToken, bool) {
	raw, ok := s.internalSecretValue(ctx, ssoTokenName)
	if !ok {
		return ssoToken{}, false
	}
	var t ssoToken
	if json.Unmarshal([]byte(raw), &t) != nil || t.AccessToken == "" {
		return ssoToken{}, false
	}
	return t, true
}

func (s *Server) dropSSOCreds() {
	s.sso.mu.Lock()
	s.sso.creds = nil
	s.sso.mu.Unlock()
}

// ssoEnv returns the environment that makes the aws command act as the signed-in role. It
// refreshes the short-lived credentials when they are close to expiring.
func (s *Server) ssoEnv(ctx context.Context) ([]string, error) {
	cfg, ok := s.loadSSO(ctx)
	if !ok || cfg.AccountID == "" || cfg.RoleName == "" {
		return nil, errors.New("AWS sign-in is not set up. Connect an account under Infrastructure")
	}
	key := cfg.AccountID + "/" + cfg.RoleName
	s.sso.mu.Lock()
	if c := s.sso.creds; c != nil && c.key == key && time.Until(c.expires) > ssoCredsMargin {
		env := c.env
		s.sso.mu.Unlock()
		return env, nil
	}
	s.sso.mu.Unlock()

	tok, ok := s.loadSSOToken(ctx)
	if !ok || time.Now().After(tok.ExpiresAt) {
		return nil, errors.New("the AWS sign-in expired. Sign in again under Infrastructure")
	}
	_, portal := s.ssoURLs(cfg.SSORegion)
	var out struct {
		RoleCredentials struct {
			AccessKeyID     string `json:"accessKeyId"`
			SecretAccessKey string `json:"secretAccessKey"`
			SessionToken    string `json:"sessionToken"`
			Expiration      int64  `json:"expiration"` // milliseconds since the epoch
		} `json:"roleCredentials"`
	}
	q := url.Values{"role_name": {cfg.RoleName}, "account_id": {cfg.AccountID}}
	status, _, err := s.ssoCall(ctx, http.MethodGet, portal+"/federation/credentials?"+q.Encode(), map[string]string{"x-amz-sso_bearer_token": tok.AccessToken}, nil, &out)
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return nil, errors.New("AWS rejected the stored sign-in. Sign in again under Infrastructure")
	}
	if err != nil {
		return nil, err
	}
	rc := out.RoleCredentials
	if rc.AccessKeyID == "" || rc.SecretAccessKey == "" || rc.SessionToken == "" {
		return nil, errors.New("AWS sent no credentials for that role")
	}
	creds := &ssoCreds{key: key, expires: time.UnixMilli(rc.Expiration), env: []string{
		"AWS_ACCESS_KEY_ID=" + rc.AccessKeyID, "AWS_SECRET_ACCESS_KEY=" + rc.SecretAccessKey, "AWS_SESSION_TOKEN=" + rc.SessionToken,
		"AWS_PROFILE=", "AWS_DEFAULT_PROFILE=", // a profile set on the server must not override the signed-in role
	}}
	s.sso.mu.Lock()
	s.sso.creds = creds
	s.sso.mu.Unlock()
	return creds.env, nil
}

// --- handlers ---

// SSOStatusJSON describes the SSO sign-in without any token.
type SSOStatusJSON struct {
	SignedIn    bool   `json:"signed_in"`
	ExpiresAt   string `json:"expires_at,omitempty"`
	StartURL    string `json:"start_url,omitempty"`
	SSORegion   string `json:"sso_region,omitempty"`
	AccountID   string `json:"account_id,omitempty"`
	AccountName string `json:"account_name,omitempty"`
	RoleName    string `json:"role_name,omitempty"`
	Active      bool   `json:"active"` // the console is using this sign-in for AWS
}

func (s *Server) handleSSOStatus(w http.ResponseWriter, r *http.Request) {
	cfg, _ := s.loadSSO(r.Context())
	out := SSOStatusJSON{StartURL: cfg.StartURL, SSORegion: cfg.SSORegion, AccountID: cfg.AccountID, AccountName: cfg.AccountName, RoleName: cfg.RoleName}
	if t, ok := s.loadSSOToken(r.Context()); ok && time.Now().Before(t.ExpiresAt) {
		out.SignedIn, out.ExpiresAt = true, t.ExpiresAt.Format(time.RFC3339)
	}
	out.Active = s.loadAWS(r.Context()).Profile == ssoProfile && s.loadAWS(r.Context()).Enabled
	writeJSON(w, http.StatusOK, out)
}

type ssoStartRequest struct {
	StartURL  string `json:"start_url"`
	SSORegion string `json:"sso_region"`
}

// SSOStartJSON is what the person needs to approve the sign-in.
type SSOStartJSON struct {
	LoginID         string `json:"login_id"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

func (s *Server) handleSSOStart(w http.ResponseWriter, r *http.Request) {
	var req ssoStartRequest
	if !readJSON(w, r, &req) {
		return
	}
	req.StartURL, req.SSORegion = strings.TrimSpace(req.StartURL), strings.TrimSpace(req.SSORegion)
	if err := checkStartURL(req.StartURL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !awsRegionRE.MatchString(req.SSORegion) {
		writeError(w, http.StatusBadRequest, "the SSO region must look like ap-south-1")
		return
	}
	oidc, _ := s.ssoURLs(req.SSORegion)
	ctx := r.Context()

	var reg struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	if _, _, err := s.ssoCall(ctx, http.MethodPost, oidc+"/client/register", nil, map[string]any{"clientName": ssoClientName, "clientType": "public", "scopes": []string{ssoScope}}, &reg); err != nil || reg.ClientID == "" {
		writeError(w, http.StatusBadGateway, "AWS did not accept the sign-in request: "+errText(err))
		return
	}
	var dev struct {
		DeviceCode              string `json:"deviceCode"`
		UserCode                string `json:"userCode"`
		VerificationURIComplete string `json:"verificationUriComplete"`
		VerificationURI         string `json:"verificationUri"`
		ExpiresIn               int    `json:"expiresIn"`
		Interval                int    `json:"interval"`
	}
	if _, _, err := s.ssoCall(ctx, http.MethodPost, oidc+"/device_authorization", nil, map[string]any{"clientId": reg.ClientID, "clientSecret": reg.ClientSecret, "startUrl": req.StartURL}, &dev); err != nil || dev.DeviceCode == "" {
		writeError(w, http.StatusBadGateway, "AWS did not start the sign-in. Check the start URL and region: "+errText(err))
		return
	}
	link := dev.VerificationURIComplete
	if link == "" {
		link = dev.VerificationURI
	}
	if err := checkVerificationURL(link); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	interval := time.Duration(dev.Interval) * time.Second
	if interval < ssoDefaultSlow {
		interval = ssoDefaultSlow
	}
	ttl := time.Duration(dev.ExpiresIn) * time.Second
	if ttl <= 0 || ttl > ssoLoginTTL {
		ttl = ssoLoginTTL
	}
	id := randomID()
	p := principalFrom(ctx)
	s.sso.mu.Lock()
	if s.sso.logins == nil {
		s.sso.logins = map[string]*ssoLogin{}
	}
	now := time.Now()
	for k, l := range s.sso.logins { // forget finished or abandoned sign-ins
		if now.After(l.expires) {
			delete(s.sso.logins, k)
		}
	}
	if len(s.sso.logins) >= ssoMaxLoginsOpen {
		s.sso.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "too many sign-ins are waiting. Finish or wait for one to expire")
		return
	}
	s.sso.logins[id] = &ssoLogin{clientID: reg.ClientID, clientSecret: reg.ClientSecret, deviceCode: dev.DeviceCode, startURL: req.StartURL, region: req.SSORegion,
		expires: now.Add(ttl), interval: interval, by: p.Name}
	s.sso.mu.Unlock()
	s.auditReq(r, "integration.aws.sso.start", "integration/aws", "sign-in started for "+req.StartURL, true)
	writeJSON(w, http.StatusOK, SSOStartJSON{LoginID: id, UserCode: dev.UserCode, VerificationURL: link, ExpiresIn: int(ttl.Seconds()), Interval: int(interval.Seconds())})
}

func errText(err error) string {
	if err == nil {
		return "no details"
	}
	return err.Error()
}

type ssoPollRequest struct {
	LoginID string `json:"login_id"`
}

// handleSSOPoll asks AWS whether the person has approved. It never blocks.
func (s *Server) handleSSOPoll(w http.ResponseWriter, r *http.Request) {
	var req ssoPollRequest
	if !readJSON(w, r, &req) {
		return
	}
	p := principalFrom(r.Context())
	s.sso.mu.Lock()
	l := s.sso.logins[req.LoginID]
	if l != nil && time.Now().After(l.expires) {
		delete(s.sso.logins, req.LoginID)
		l = nil
	}
	if l == nil || l.by != p.Name { // someone else's sign-in looks the same as none, and is left alone
		s.sso.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"status": "expired"})
		return
	}
	if time.Since(l.lastPoll) < l.interval { // do not hammer AWS faster than it asked
		s.sso.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"status": "pending"})
		return
	}
	l.lastPoll = time.Now()
	login := *l
	s.sso.mu.Unlock()

	oidc, _ := s.ssoURLs(login.region)
	var tok struct {
		AccessToken string `json:"accessToken"`
		ExpiresIn   int    `json:"expiresIn"`
	}
	_, code, err := s.ssoCall(r.Context(), http.MethodPost, oidc+"/token", nil, map[string]any{
		"clientId": login.clientID, "clientSecret": login.clientSecret, "grantType": ssoDeviceGrant, "deviceCode": login.deviceCode}, &tok)
	if err != nil {
		switch strings.ToLower(code) {
		case "authorization_pending", "authorizationpendingexception":
			writeJSON(w, http.StatusOK, map[string]string{"status": "pending"})
		case "slow_down", "slowdownexception":
			s.sso.mu.Lock()
			l.interval += 5 * time.Second
			s.sso.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]string{"status": "pending"})
		case "expired_token", "expiredtokenexception":
			s.endLogin(req.LoginID)
			writeJSON(w, http.StatusOK, map[string]string{"status": "expired"})
		case "access_denied", "accessdeniedexception":
			s.endLogin(req.LoginID)
			s.auditReq(r, "integration.aws.sso.denied", "integration/aws", "the sign-in was denied in AWS", false)
			writeJSON(w, http.StatusOK, map[string]string{"status": "denied"})
		default:
			writeError(w, http.StatusBadGateway, err.Error())
		}
		return
	}
	if tok.AccessToken == "" {
		writeError(w, http.StatusBadGateway, "AWS sent no token")
		return
	}
	life := time.Duration(tok.ExpiresIn) * time.Second
	if life <= 0 || life > 24*time.Hour {
		life = time.Hour
	}
	val, _ := json.Marshal(ssoToken{AccessToken: tok.AccessToken, ExpiresAt: time.Now().Add(life)})
	if _, _, err := s.putSecret(r.Context(), p.Name, ssoTokenName, string(val), "AWS SSO access token"); err != nil {
		s.internalError(w, "storing the sign-in", err)
		return
	}
	// Keep the start address for the next steps; the account and role are chosen next.
	cfg := ssoConfig{StartURL: login.startURL, SSORegion: login.region, UpdatedAt: time.Now().UTC(), UpdatedBy: p.Name}
	if old, ok := s.loadSSO(r.Context()); ok && old.StartURL == cfg.StartURL && old.SSORegion == cfg.SSORegion {
		cfg.AccountID, cfg.AccountName, cfg.RoleName = old.AccountID, old.AccountName, old.RoleName
	}
	if err := s.docs.Put(r.Context(), kindSettings, idAWSSSO, cfg); err != nil {
		s.internalError(w, "saving the sign-in", err)
		return
	}
	s.endLogin(req.LoginID)
	s.dropSSOCreds()
	s.auditReq(r, "integration.aws.sso.signin", "integration/aws", "signed in to "+login.startURL, true)
	writeJSON(w, http.StatusOK, map[string]string{"status": "authorized"})
}

func (s *Server) endLogin(id string) {
	s.sso.mu.Lock()
	delete(s.sso.logins, id)
	s.sso.mu.Unlock()
}

// SSOAccountJSON is an AWS account the signed-in person can use.
type SSOAccountJSON struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
}

func (s *Server) portalToken(ctx context.Context) (ssoConfig, ssoToken, string, error) {
	cfg, ok := s.loadSSO(ctx)
	tok, ok2 := s.loadSSOToken(ctx)
	if !ok || !ok2 || time.Now().After(tok.ExpiresAt) {
		return cfg, tok, "", errors.New("sign in to AWS first")
	}
	_, portal := s.ssoURLs(cfg.SSORegion)
	return cfg, tok, portal, nil
}

func (s *Server) handleSSOAccounts(w http.ResponseWriter, r *http.Request) {
	_, tok, portal, err := s.portalToken(r.Context())
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	var out []SSOAccountJSON
	next := ""
	for page := 0; page < 20; page++ {
		var resp struct {
			AccountList []struct {
				AccountID    string `json:"accountId"`
				AccountName  string `json:"accountName"`
				EmailAddress string `json:"emailAddress"`
			} `json:"accountList"`
			NextToken string `json:"nextToken"`
		}
		q := url.Values{"max_result": {"100"}}
		if next != "" {
			q.Set("next_token", next)
		}
		if _, _, err := s.ssoCall(r.Context(), http.MethodGet, portal+"/assignment/accounts?"+q.Encode(), map[string]string{"x-amz-sso_bearer_token": tok.AccessToken}, nil, &resp); err != nil {
			writeError(w, http.StatusBadGateway, "could not list accounts: "+err.Error())
			return
		}
		for _, a := range resp.AccountList {
			out = append(out, SSOAccountJSON{ID: a.AccountID, Name: a.AccountName, Email: a.EmailAddress})
		}
		if resp.NextToken == "" {
			break
		}
		next = resp.NextToken
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

var awsAccountIDRE = regexp.MustCompile(`^[0-9]{12}$`)

func (s *Server) handleSSORoles(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("account_id")
	if !awsAccountIDRE.MatchString(id) {
		writeError(w, http.StatusBadRequest, "account_id must be 12 digits")
		return
	}
	_, tok, portal, err := s.portalToken(r.Context())
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	var resp struct {
		RoleList []struct {
			RoleName string `json:"roleName"`
		} `json:"roleList"`
	}
	q := url.Values{"account_id": {id}, "max_result": {"100"}}
	if _, _, err := s.ssoCall(r.Context(), http.MethodGet, portal+"/assignment/roles?"+q.Encode(), map[string]string{"x-amz-sso_bearer_token": tok.AccessToken}, nil, &resp); err != nil {
		writeError(w, http.StatusBadGateway, "could not list roles: "+err.Error())
		return
	}
	roles := []string{}
	for _, ro := range resp.RoleList {
		roles = append(roles, ro.RoleName)
	}
	sort.Strings(roles)
	writeJSON(w, http.StatusOK, map[string]any{"roles": roles})
}

type ssoConnectRequest struct {
	AccountID   string `json:"account_id"`
	AccountName string `json:"account_name"`
	RoleName    string `json:"role_name"`
	Region      string `json:"region"`
}

var awsRoleNameRE = regexp.MustCompile(`^[A-Za-z0-9+=,.@_-]{1,64}$`)

// handleSSOConnect makes the signed-in account and role the console's AWS connection. It proves
// it works (a real identity call) before saving.
func (s *Server) handleSSOConnect(w http.ResponseWriter, r *http.Request) {
	var req ssoConnectRequest
	if !readJSON(w, r, &req) {
		return
	}
	req.Region = strings.TrimSpace(req.Region)
	if !awsAccountIDRE.MatchString(req.AccountID) || !awsRoleNameRE.MatchString(req.RoleName) || !awsRegionRE.MatchString(req.Region) {
		writeError(w, http.StatusBadRequest, "choose an account, a role and a region such as ap-south-1")
		return
	}
	cfg, _, _, err := s.portalToken(r.Context())
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	p := principalFrom(r.Context())
	cfg.AccountID, cfg.AccountName, cfg.RoleName = req.AccountID, truncate(req.AccountName, 80), req.RoleName
	cfg.UpdatedAt, cfg.UpdatedBy = time.Now().UTC(), p.Name

	// Try the new choice before keeping it: put it in place, and put the old one back on failure.
	prev, hadPrev := s.loadSSO(r.Context())
	if err := s.docs.Put(r.Context(), kindSettings, idAWSSSO, cfg); err != nil {
		s.internalError(w, "saving the sign-in", err)
		return
	}
	s.dropSSOCreds()
	st := awsSettings{Enabled: true, Profile: ssoProfile, Region: req.Region, UpdatedAt: time.Now().UTC(), UpdatedBy: p.Name}
	if !s.applyAWSSettings(w, r, st) {
		if hadPrev {
			s.docs.Put(r.Context(), kindSettings, idAWSSSO, prev)
		} else {
			s.docs.Delete(r.Context(), kindSettings, idAWSSSO)
		}
		s.dropSSOCreds()
	}
}

func (s *Server) handleSSODisconnect(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.loadAWS(ctx).Profile == ssoProfile {
		if err := s.docs.Put(ctx, kindSettings, idAWS, awsSettings{UpdatedAt: time.Now().UTC(), UpdatedBy: principalFrom(ctx).Name}); err != nil {
			s.internalError(w, "saving the settings", err)
			return
		}
	}
	for _, e := range []error{s.docs.Delete(ctx, kindSecret, ssoTokenName), s.docs.Delete(ctx, kindSettings, idAWSSSO)} {
		if e != nil && !errors.Is(e, docstore.ErrNotFound) {
			s.internalError(w, "forgetting the sign-in", e)
			return
		}
	}
	s.dropSSOCreds()
	s.awsCache.mu.Lock()
	s.awsCache.key, s.awsCache.cost = "", nil
	s.awsCache.mu.Unlock()
	s.auditReq(r, "integration.aws.sso.signout", "integration/aws", "the stored sign-in was deleted", true)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- checks ---

// checkStartURL accepts the address people copy from the IAM Identity Center page.
func checkStartURL(raw string) error {
	u, err := url.Parse(raw)
	if raw == "" || len(raw) > ssoStartURLMax || err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" {
		return errors.New("the start URL must look like https://d-1234567890.awsapps.com/start")
	}
	host := u.Hostname()
	if !strings.HasSuffix(host, ".awsapps.com") && !strings.HasSuffix(host, ".amazonaws.com") && !strings.HasSuffix(host, ".aws") && !strings.HasSuffix(host, ".app.aws") && !strings.HasSuffix(host, ".amazonaws.com.cn") {
		return errors.New("that is not an AWS access portal address (it should end in awsapps.com)")
	}
	return nil
}

// checkVerificationURL makes sure the link shown to the person is a web link, not something that
// runs script, whatever the sign-in server answers.
func checkVerificationURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost"))) {
		return errors.New("AWS sent a sign-in link that is not a web address")
	}
	return nil
}

func randomID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
