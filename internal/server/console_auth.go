package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/forgelinehq/forgeline/internal/authn"
	"github.com/forgelinehq/forgeline/internal/docstore"
)

// Document kinds kept in the console's docstore.
const (
	kindUser    = "user"
	kindSession = "session"
	kindAudit   = "audit"
)

const (
	sessionCookie  = "forgeline_session"
	sessionTTL     = 12 * time.Hour
	csrfHeader     = "X-Forgeline-Request"
	maxJSONBody    = 1 << 20
	maxLoginFails  = 5
	maxLockout     = 15 * time.Minute
	baseLockout    = 30 * time.Second
	tokenPrincipal = "token"
)

// Principal is who is making a request.
type Principal struct {
	// Name is the username, or "token" for the shared bearer token.
	Name string
	Role authn.Role
	// UserID is empty for the bearer token.
	UserID string
	// ViaCookie is true for browser sessions, which need CSRF protection.
	ViaCookie bool
}

type principalKey struct{}

func principalFrom(ctx context.Context) Principal {
	p, _ := ctx.Value(principalKey{}).(Principal)
	return p
}

// User is a stored account. Its ID is the lower-cased username, which makes usernames unique.
type User struct {
	ID           string     `json:"id"`
	Username     string     `json:"username"`
	Name         string     `json:"name,omitempty"`
	Role         authn.Role `json:"role"`
	PasswordHash string     `json:"password_hash"`
	Disabled     bool       `json:"disabled,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	CreatedBy    string     `json:"created_by,omitempty"`
	LastLoginAt  *time.Time `json:"last_login_at,omitempty"`
}

// UserJSON is what the API shows of a User: never the hash.
type UserJSON struct {
	ID          string     `json:"id"`
	Username    string     `json:"username"`
	Name        string     `json:"name,omitempty"`
	Role        authn.Role `json:"role"`
	Disabled    bool       `json:"disabled"`
	CreatedAt   time.Time  `json:"created_at"`
	CreatedBy   string     `json:"created_by,omitempty"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

func (u User) json() UserJSON {
	return UserJSON{ID: u.ID, Username: u.Username, Name: u.Name, Role: u.Role, Disabled: u.Disabled,
		CreatedAt: u.CreatedAt, CreatedBy: u.CreatedBy, LastLoginAt: u.LastLoginAt}
}

type sessionDoc struct {
	UserID    string    `json:"user_id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	IP        string    `json:"ip,omitempty"`
	Agent     string    `json:"agent,omitempty"`
}

// AuditEvent is one entry of the audit log.
type AuditEvent struct {
	ID      string    `json:"id"`
	Time    time.Time `json:"time"`
	Actor   string    `json:"actor"`
	Role    string    `json:"role,omitempty"`
	Action  string    `json:"action"`
	Target  string    `json:"target,omitempty"`
	Detail  string    `json:"detail,omitempty"`
	Allowed bool      `json:"allowed"`
	IP      string    `json:"ip,omitempty"`
}

// throttle slows password guessing per account and per address.
type throttle struct {
	mu sync.Mutex
	m  map[string]*attempts
}

type attempts struct {
	fails int
	until time.Time
}

func (t *throttle) blockedFor(key string, now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if a := t.m[key]; a != nil && now.Before(a.until) {
		return a.until.Sub(now)
	}
	return 0
}

func (t *throttle) fail(key string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]*attempts{}
	}
	a := t.m[key]
	if a == nil {
		a = &attempts{}
		t.m[key] = a
	}
	a.fails++
	if a.fails >= maxLoginFails {
		d := baseLockout << min(a.fails-maxLoginFails, 5)
		a.until = now.Add(min(d, maxLockout))
	}
}

func (t *throttle) clear(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, key)
}

// clientIP is the address of the peer. Forwarding headers are not trusted: anyone can send them.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// audit appends an event. A failure to write is logged and swallowed for events about
// allowed actions that already happened, but callers that must not proceed without a record
// use auditMust.
func (s *Server) audit(ctx context.Context, p Principal, ip, action, target, detail string, allowed bool) error {
	now := time.Now().UTC()
	ev := AuditEvent{
		ID: fmt.Sprintf("%020d-%s", now.UnixNano(), authn.NewID()[:6]), Time: now,
		Actor: p.Name, Role: string(p.Role), Action: action, Target: target, Detail: detail, Allowed: allowed, IP: ip,
	}
	if err := s.docs.Put(context.WithoutCancel(ctx), kindAudit, ev.ID, ev); err != nil {
		s.log.Error("writing audit event", "action", action, "err", err)
		return err
	}
	return nil
}

func (s *Server) auditReq(r *http.Request, action, target, detail string, allowed bool) {
	s.audit(r.Context(), principalFrom(r.Context()), clientIP(r), action, target, detail, allowed)
}

// requireRole wraps a handler: it authenticates, checks the role, and records refusals.
func (s *Server) requireRole(min authn.Role, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.authenticate(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "sign in required")
			return
		}
		if p.ViaCookie && isUnsafe(r.Method) {
			if r.Header.Get(csrfHeader) == "" {
				writeError(w, http.StatusForbidden, "missing "+csrfHeader+" header")
				return
			}
			if o := r.Header.Get("Origin"); o != "" && !sameOrigin(o, r.Host) {
				s.audit(r.Context(), p, clientIP(r), "request.cross-origin", r.Method+" "+r.URL.Path, "origin "+o, false)
				writeError(w, http.StatusForbidden, "cross-origin request refused")
				return
			}
		}
		if !p.Role.AtLeast(min) {
			s.audit(r.Context(), p, clientIP(r), "access.denied", r.Method+" "+r.URL.Path, "needs "+string(min), false)
			writeError(w, http.StatusForbidden, "your role ("+string(p.Role)+") cannot do this; "+string(min)+" is required")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

func isUnsafe(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

func sameOrigin(origin, host string) bool {
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, host)
}

// authenticate resolves the caller from a bearer token or a session cookie.
func (s *Server) authenticate(r *http.Request) (Principal, bool) {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		got := sha256.Sum256([]byte(strings.TrimPrefix(h, "Bearer ")))
		want := sha256.Sum256([]byte(s.cfg.Token))
		if subtle.ConstantTimeCompare(got[:], want[:]) == 1 {
			return Principal{Name: tokenPrincipal, Role: authn.Admin}, true
		}
		return Principal{}, false
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return Principal{}, false
	}
	var sess sessionDoc
	id := authn.TokenID(c.Value)
	if err := s.docs.Get(r.Context(), kindSession, id, &sess); err != nil {
		return Principal{}, false
	}
	if time.Now().After(sess.ExpiresAt) {
		s.docs.Delete(r.Context(), kindSession, id)
		return Principal{}, false
	}
	var u User
	if err := s.docs.Get(r.Context(), kindUser, sess.UserID, &u); err != nil || u.Disabled {
		return Principal{}, false
	}
	return Principal{Name: u.Username, Role: u.Role, UserID: u.ID, ViaCookie: true}, true
}

// readJSON decodes a size-limited JSON body and rejects unknown fields.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	// A cross-site HTML form can send text/plain but not application/json without a preflight.
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "send Content-Type: application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body is too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		}
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid JSON: unexpected data after the object")
		return false
	}
	return true
}

func (s *Server) cookieSecure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u User) error {
	token, id, err := authn.NewToken()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	sess := sessionDoc{UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(sessionTTL), IP: clientIP(r), Agent: truncate(r.UserAgent(), 200)}
	if err := s.docs.Put(r.Context(), kindSession, id, sess); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: s.cookieSecure(r), Expires: sess.ExpiresAt, MaxAge: int(sessionTTL.Seconds()),
	})
	return nil
}

// dropSessions ends every session of a user, for password changes, disabling and deletion.
func (s *Server) dropSessions(ctx context.Context, userID string) {
	docs, err := s.docs.List(ctx, kindSession, docstore.ListOptions{})
	if err != nil {
		s.log.Error("listing sessions", "err", err)
		return
	}
	for _, d := range docs {
		var sess sessionDoc
		if json.Unmarshal(d.Body, &sess) == nil && sess.UserID == userID {
			s.docs.Delete(ctx, kindSession, d.ID)
		}
	}
}

type authStateJSON struct {
	SetupNeeded   bool      `json:"setup_needed"`
	Authenticated bool      `json:"authenticated"`
	User          *UserJSON `json:"user,omitempty"`
	// Actor is the name shown for the shared bearer token, which has no user record.
	Actor string `json:"actor,omitempty"`
	Role  string `json:"role,omitempty"`
}

func (s *Server) handleAuthState(w http.ResponseWriter, r *http.Request) {
	n, err := s.docs.Count(r.Context(), kindUser)
	if err != nil {
		s.internalError(w, "reading accounts", err)
		return
	}
	out := authStateJSON{SetupNeeded: n == 0}
	if p, ok := s.authenticate(r); ok {
		out.Authenticated, out.Actor, out.Role = true, p.Name, string(p.Role)
		if p.UserID != "" {
			var u User
			if s.docs.Get(r.Context(), kindUser, p.UserID, &u) == nil {
				j := u.json()
				out.User = &j
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

type setupRequest struct {
	Token    string `json:"token"`
	Username string `json:"username"`
	Name     string `json:"name"`
	Password string `json:"password"`
}

// handleSetup creates the first administrator. It needs the server's bearer token, so the first
// visitor to an exposed port cannot take the server over.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	var req setupRequest
	if !readJSON(w, r, &req) {
		return
	}
	ip := clientIP(r)
	key := "setup|" + ip
	if d := s.throttle.blockedFor(key, time.Now()); d > 0 {
		writeError(w, http.StatusTooManyRequests, "too many attempts; try again in "+d.Round(time.Second).String())
		return
	}
	got := sha256.Sum256([]byte(req.Token))
	want := sha256.Sum256([]byte(s.cfg.Token))
	if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		s.throttle.fail(key, time.Now())
		s.audit(r.Context(), Principal{Name: "anonymous"}, ip, "setup.refused", "", "wrong server token", false)
		writeError(w, http.StatusUnauthorized, "the server token is wrong")
		return
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if n, _ := s.docs.Count(r.Context(), kindUser); n > 0 {
		writeError(w, http.StatusConflict, "setup is already done")
		return
	}
	u, status, err := s.newUser(req.Username, req.Name, req.Password, authn.Admin, "setup")
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	if err := s.docs.Create(r.Context(), kindUser, u.ID, u); err != nil {
		s.internalError(w, "creating the account", err)
		return
	}
	s.audit(r.Context(), Principal{Name: u.Username, Role: u.Role}, ip, "setup.complete", "user/"+u.ID, "first administrator created", true)
	if err := s.startSession(w, r, u); err != nil {
		s.internalError(w, "starting a session", err)
		return
	}
	writeJSON(w, http.StatusCreated, u.json())
}

// newUser validates and builds an account.
func (s *Server) newUser(username, name, password string, role authn.Role, createdBy string) (User, int, error) {
	username = strings.TrimSpace(username)
	if err := authn.ValidUsername(username); err != nil {
		return User{}, http.StatusBadRequest, err
	}
	if !role.Valid() {
		return User{}, http.StatusBadRequest, errors.New("role must be viewer, developer or admin")
	}
	if err := authn.CheckPassword(username, password); err != nil {
		return User{}, http.StatusBadRequest, err
	}
	if len(name) > 100 {
		return User{}, http.StatusBadRequest, errors.New("name is too long")
	}
	hash, err := authn.HashPassword(password)
	if err != nil {
		return User{}, http.StatusInternalServerError, err
	}
	return User{ID: strings.ToLower(username), Username: username, Name: strings.TrimSpace(name), Role: role,
		PasswordHash: hash, CreatedAt: time.Now().UTC(), CreatedBy: createdBy}, 0, nil
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !readJSON(w, r, &req) {
		return
	}
	ip := clientIP(r)
	id := strings.ToLower(strings.TrimSpace(req.Username))
	keys := []string{"user|" + id, "ip|" + ip}
	now := time.Now()
	for _, k := range keys {
		if d := s.throttle.blockedFor(k, now); d > 0 {
			s.audit(r.Context(), Principal{Name: id}, ip, "login.throttled", "", "", false)
			w.Header().Set("Retry-After", fmt.Sprint(int(d.Seconds())+1))
			writeError(w, http.StatusTooManyRequests, "too many failed attempts; try again in "+d.Round(time.Second).String())
			return
		}
	}
	var u User
	err := s.docs.Get(r.Context(), kindUser, id, &u)
	switch {
	case errors.Is(err, docstore.ErrNotFound):
		authn.DummyVerify(req.Password)
	case err != nil:
		s.internalError(w, "reading the account", err)
		return
	}
	ok, rehash := false, false
	if err == nil {
		ok, rehash = authn.VerifyPassword(req.Password, u.PasswordHash)
	}
	if !ok || u.Disabled {
		for _, k := range keys {
			s.throttle.fail(k, now)
		}
		reason := "wrong username or password"
		if ok && u.Disabled {
			reason = "account disabled"
		}
		s.audit(r.Context(), Principal{Name: id}, ip, "login.failed", "", reason, false)
		// One message for every failure, so the response does not say which accounts exist.
		writeError(w, http.StatusUnauthorized, "wrong username or password")
		return
	}
	for _, k := range keys {
		s.throttle.clear(k)
	}
	t := time.Now().UTC()
	u.LastLoginAt = &t
	if rehash {
		if h, err := authn.HashPassword(req.Password); err == nil {
			u.PasswordHash = h
		}
	}
	if err := s.docs.Put(r.Context(), kindUser, u.ID, u); err != nil {
		s.internalError(w, "updating the account", err)
		return
	}
	if err := s.startSession(w, r, u); err != nil {
		s.internalError(w, "starting a session", err)
		return
	}
	s.audit(r.Context(), Principal{Name: u.Username, Role: u.Role}, ip, "login", "user/"+u.ID, "", true)
	writeJSON(w, http.StatusOK, u.json())
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.docs.Delete(r.Context(), kindSession, authn.TokenID(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	s.auditReq(r, "logout", "", "", true)
	w.WriteHeader(http.StatusNoContent)
}

// --- user management (admin) ---

func (s *Server) loadUsers(ctx context.Context) ([]User, error) {
	docs, err := s.docs.List(ctx, kindUser, docstore.ListOptions{})
	if err != nil {
		return nil, err
	}
	return docstore.Decode[User](docs)
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.loadUsers(r.Context())
	if err != nil {
		s.internalError(w, "listing users", err)
		return
	}
	out := make([]UserJSON, len(users))
	for i, u := range users {
		out[i] = u.json()
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

type createUserRequest struct {
	Username string     `json:"username"`
	Name     string     `json:"name"`
	Password string     `json:"password"`
	Role     authn.Role `json:"role"`
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if !readJSON(w, r, &req) {
		return
	}
	p := principalFrom(r.Context())
	u, status, err := s.newUser(req.Username, req.Name, req.Password, req.Role, p.Name)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	if err := s.docs.Create(r.Context(), kindUser, u.ID, u); errors.Is(err, docstore.ErrExists) {
		writeError(w, http.StatusConflict, "that username is taken")
		return
	} else if err != nil {
		s.internalError(w, "creating the account", err)
		return
	}
	s.auditReq(r, "user.create", "user/"+u.ID, "role "+string(u.Role), true)
	writeJSON(w, http.StatusCreated, u.json())
}

type updateUserRequest struct {
	Name     *string     `json:"name"`
	Role     *authn.Role `json:"role"`
	Disabled *bool       `json:"disabled"`
	Password *string     `json:"password"`
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(r.PathValue("id"))
	var req updateUserRequest
	if !readJSON(w, r, &req) {
		return
	}
	s.userMu.Lock()
	defer s.userMu.Unlock()
	var u User
	if err := s.docs.Get(r.Context(), kindUser, id, &u); errors.Is(err, docstore.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such user")
		return
	} else if err != nil {
		s.internalError(w, "reading the account", err)
		return
	}
	wasAdmin := u.Role == authn.Admin && !u.Disabled
	var changes []string
	if req.Name != nil {
		if len(*req.Name) > 100 {
			writeError(w, http.StatusBadRequest, "name is too long")
			return
		}
		u.Name = strings.TrimSpace(*req.Name)
		changes = append(changes, "name")
	}
	if req.Role != nil {
		if !req.Role.Valid() {
			writeError(w, http.StatusBadRequest, "role must be viewer, developer or admin")
			return
		}
		if u.Role != *req.Role {
			changes = append(changes, fmt.Sprintf("role %s->%s", u.Role, *req.Role))
		}
		u.Role = *req.Role
	}
	if req.Disabled != nil {
		if u.Disabled != *req.Disabled {
			changes = append(changes, fmt.Sprintf("disabled=%v", *req.Disabled))
		}
		u.Disabled = *req.Disabled
	}
	resetSessions := u.Disabled
	if req.Password != nil {
		if err := authn.CheckPassword(u.Username, *req.Password); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		h, err := authn.HashPassword(*req.Password)
		if err != nil {
			s.internalError(w, "hashing the password", err)
			return
		}
		u.PasswordHash = h
		resetSessions = true
		changes = append(changes, "password reset")
	}
	if len(changes) == 0 {
		writeJSON(w, http.StatusOK, u.json())
		return
	}
	if wasAdmin && !(u.Role == authn.Admin && !u.Disabled) {
		if n, err := s.activeAdmins(r.Context()); err != nil || n <= 1 {
			writeError(w, http.StatusConflict, "this is the last active administrator; make another one first")
			return
		}
	}
	if err := s.docs.Put(r.Context(), kindUser, u.ID, u); err != nil {
		s.internalError(w, "saving the account", err)
		return
	}
	if resetSessions {
		s.dropSessions(r.Context(), u.ID)
		if u.Disabled {
			s.dropTokensOf(r.Context(), u.ID) // re-enabling a person must not bring old tokens back
		}
	}
	s.auditReq(r, "user.update", "user/"+u.ID, strings.Join(changes, ", "), true)
	writeJSON(w, http.StatusOK, u.json())
}

func (s *Server) activeAdmins(ctx context.Context) (int, error) {
	users, err := s.loadUsers(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, u := range users {
		if u.Role == authn.Admin && !u.Disabled {
			n++
		}
	}
	return n, nil
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(r.PathValue("id"))
	s.userMu.Lock()
	defer s.userMu.Unlock()
	var u User
	if err := s.docs.Get(r.Context(), kindUser, id, &u); errors.Is(err, docstore.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such user")
		return
	} else if err != nil {
		s.internalError(w, "reading the account", err)
		return
	}
	if u.Role == authn.Admin && !u.Disabled {
		if n, err := s.activeAdmins(r.Context()); err != nil || n <= 1 {
			writeError(w, http.StatusConflict, "this is the last active administrator; make another one first")
			return
		}
	}
	if err := s.docs.Delete(r.Context(), kindUser, u.ID); err != nil {
		s.internalError(w, "deleting the account", err)
		return
	}
	s.dropSessions(r.Context(), u.ID)
	s.dropTokensOf(r.Context(), u.ID)
	s.auditReq(r, "user.delete", "user/"+u.ID, "", true)
	w.WriteHeader(http.StatusNoContent)
}

type changePasswordRequest struct {
	Current string `json:"current"`
	New     string `json:"new"`
}

func (s *Server) handleChangeOwnPassword(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p.UserID == "" {
		writeError(w, http.StatusBadRequest, "the shared server token has no password")
		return
	}
	var req changePasswordRequest
	if !readJSON(w, r, &req) {
		return
	}
	var u User
	if err := s.docs.Get(r.Context(), kindUser, p.UserID, &u); err != nil {
		s.internalError(w, "reading the account", err)
		return
	}
	if ok, _ := authn.VerifyPassword(req.Current, u.PasswordHash); !ok {
		s.auditReq(r, "password.change", "user/"+u.ID, "wrong current password", false)
		writeError(w, http.StatusForbidden, "the current password is wrong")
		return
	}
	if err := authn.CheckPassword(u.Username, req.New); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h, err := authn.HashPassword(req.New)
	if err != nil {
		s.internalError(w, "hashing the password", err)
		return
	}
	u.PasswordHash = h
	if err := s.docs.Put(r.Context(), kindUser, u.ID, u); err != nil {
		s.internalError(w, "saving the account", err)
		return
	}
	s.dropSessions(r.Context(), u.ID) // signs out every device, including this one
	s.auditReq(r, "password.change", "user/"+u.ID, "", true)
	w.WriteHeader(http.StatusNoContent)
}

// --- audit log (admin) ---

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 100
	if v := q.Get("limit"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &limit); err != nil || limit < 1 || limit > 1000 {
			writeError(w, http.StatusBadRequest, "limit must be 1 to 1000")
			return
		}
	}
	// Filters are applied after reading, so read more than the page when filtering.
	fetch := limit
	filtering := q.Get("actor") != "" || q.Get("action") != "" || q.Get("denied") != ""
	if filtering {
		fetch = 5000
	}
	docs, err := s.docs.List(r.Context(), kindAudit, docstore.ListOptions{Desc: true, Limit: fetch})
	if err != nil {
		s.internalError(w, "reading the audit log", err)
		return
	}
	events, err := docstore.Decode[AuditEvent](docs)
	if err != nil {
		s.internalError(w, "reading the audit log", err)
		return
	}
	actor, action := strings.ToLower(q.Get("actor")), q.Get("action")
	out := make([]AuditEvent, 0, limit)
	for _, e := range events {
		if actor != "" && strings.ToLower(e.Actor) != actor {
			continue
		}
		if action != "" && !strings.HasPrefix(e.Action, action) {
			continue
		}
		if q.Get("denied") == "1" && e.Allowed {
			continue
		}
		out = append(out, e)
		if len(out) == limit {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}
