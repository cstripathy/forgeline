package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/forgelinehq/forgeline/internal/authn"
	"github.com/forgelinehq/forgeline/internal/docstore"
)

// Personal API tokens (design note 020). They exist for the MCP endpoint only: a token belongs to a
// person, carries a role no higher than theirs, expires, and is stored as a hash. The shared server
// token and cookie sessions are never accepted by /mcp, and a token cannot create tokens.

const (
	kindAPIToken     = "apitoken"
	apiTokenPrefix   = "flt_"
	maxTokensPerUser = 10
	defaultTokenDays = 90
	maxTokenDays     = 365
	touchEvery       = 5 * time.Minute
)

// APIToken is a stored token. The secret itself is never stored.
type APIToken struct {
	ID        string     `json:"id"` // the token's SHA-256
	UserID    string     `json:"user_id"`
	Owner     string     `json:"owner"`
	Name      string     `json:"name"`
	Role      authn.Role `json:"role"` // the most this token may do; the owner's role may be lower
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	LastUsed  *time.Time `json:"last_used,omitempty"`
}

type apiTokenState struct {
	mu sync.Mutex
}

func newAPIToken() (secret, id string, err error) {
	t, _, err := authn.NewToken()
	if err != nil {
		return "", "", err
	}
	secret = apiTokenPrefix + t
	return secret, authn.TokenID(secret), nil
}

type createTokenRequest struct {
	Name string     `json:"name"`
	Role authn.Role `json:"role"`
	Days int        `json:"days"`
}

type createTokenReply struct {
	APIToken
	// Token is the secret. It is shown once.
	Token string `json:"token"`
}

func validTokenRole(r authn.Role) bool {
	return r == authn.Viewer || r == authn.Developer || r == authn.Admin
}

// handleCreateToken makes a token for the signed-in person. It needs a browser session: a token
// (or the shared server token) cannot mint more tokens.
func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if !p.ViaCookie || p.UserID == "" {
		writeError(w, http.StatusForbidden, "sign in to the console as a person to create a token")
		return
	}
	var req createTokenRequest
	if !readJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Role == "" {
		req.Role = p.Role
	}
	if len(req.Name) < 1 || len(req.Name) > 60 {
		writeError(w, http.StatusBadRequest, "give the token a name of up to 60 characters, such as the tool that will use it")
		return
	}
	if !validTokenRole(req.Role) {
		writeError(w, http.StatusBadRequest, "role must be viewer, developer or admin")
		return
	}
	if !p.Role.AtLeast(req.Role) {
		writeError(w, http.StatusForbidden, "a token cannot have a higher role than you have")
		return
	}
	if req.Days == 0 {
		req.Days = defaultTokenDays
	}
	if req.Days < 1 || req.Days > maxTokenDays {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("days is 1 to %d", maxTokenDays))
		return
	}
	s.apiTokens.mu.Lock()
	defer s.apiTokens.mu.Unlock()
	mine, err := s.tokensOf(r.Context(), p.UserID)
	if err != nil {
		s.internalError(w, "reading your tokens", err)
		return
	}
	if len(mine) >= maxTokensPerUser {
		writeError(w, http.StatusConflict, fmt.Sprintf("you already have %d tokens; revoke one first", maxTokensPerUser))
		return
	}
	secret, id, err := newAPIToken()
	if err != nil {
		s.internalError(w, "creating the token", err)
		return
	}
	now := time.Now().UTC()
	t := APIToken{ID: id, UserID: p.UserID, Owner: p.Name, Name: req.Name, Role: req.Role, CreatedAt: now, ExpiresAt: now.AddDate(0, 0, req.Days)}
	if err := s.docs.Put(r.Context(), kindAPIToken, id, t); err != nil {
		s.internalError(w, "saving the token", err)
		return
	}
	s.auditReq(r, "token.create", "token/"+id[:12], fmt.Sprintf("%q, role %s, %d days", t.Name, t.Role, req.Days), true)
	writeJSON(w, http.StatusCreated, createTokenReply{APIToken: t, Token: secret})
}

func (s *Server) tokensOf(ctx context.Context, userID string) ([]APIToken, error) {
	docs, err := s.docs.List(ctx, kindAPIToken, docstore.ListOptions{})
	if err != nil {
		return nil, err
	}
	all, err := docstore.Decode[APIToken](docs)
	if err != nil {
		return nil, err
	}
	var out []APIToken
	for _, t := range all {
		if t.UserID == userID {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// handleListTokens lists your tokens; an administrator may ask for everyone's with ?all=1.
func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	var out []APIToken
	if r.URL.Query().Get("all") == "1" && p.Role.AtLeast(authn.Admin) {
		docs, err := s.docs.List(r.Context(), kindAPIToken, docstore.ListOptions{})
		if err == nil {
			out, err = docstore.Decode[APIToken](docs)
		}
		if err != nil {
			s.internalError(w, "reading the tokens", err)
			return
		}
		sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	} else {
		if p.UserID == "" {
			writeJSON(w, http.StatusOK, map[string]any{"tokens": []APIToken{}})
			return
		}
		var err error
		if out, err = s.tokensOf(r.Context(), p.UserID); err != nil {
			s.internalError(w, "reading your tokens", err)
			return
		}
	}
	if out == nil {
		out = []APIToken{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

// handleRevokeToken deletes a token. Its owner or an administrator may.
func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	id := r.PathValue("id")
	var t APIToken
	if err := s.docs.Get(r.Context(), kindAPIToken, id, &t); errors.Is(err, docstore.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such token")
		return
	} else if err != nil {
		s.internalError(w, "reading the token", err)
		return
	}
	if t.UserID != p.UserID && !p.Role.AtLeast(authn.Admin) {
		writeError(w, http.StatusNotFound, "no such token") // not yours: do not confirm it exists
		return
	}
	if err := s.docs.Delete(r.Context(), kindAPIToken, id); err != nil {
		s.internalError(w, "revoking the token", err)
		return
	}
	s.auditReq(r, "token.revoke", "token/"+id[:12], fmt.Sprintf("%q of %s", t.Name, t.Owner), true)
	w.WriteHeader(http.StatusNoContent)
}

// authenticateToken resolves a personal token to the person behind it, with the lower of the token's
// role and the person's current role. A disabled or deleted user, an expired token and an unknown
// one all fail the same way.
func (s *Server) authenticateToken(ctx context.Context, secret string) (Principal, APIToken, bool) {
	if !strings.HasPrefix(secret, apiTokenPrefix) || len(secret) > 200 {
		return Principal{}, APIToken{}, false
	}
	var t APIToken
	if err := s.docs.Get(ctx, kindAPIToken, authn.TokenID(secret), &t); err != nil {
		return Principal{}, APIToken{}, false
	}
	now := time.Now()
	if now.After(t.ExpiresAt) {
		return Principal{}, APIToken{}, false
	}
	var u User
	if err := s.docs.Get(ctx, kindUser, t.UserID, &u); err != nil || u.Disabled {
		return Principal{}, APIToken{}, false
	}
	role := t.Role
	if !u.Role.AtLeast(role) {
		role = u.Role
	}
	if t.LastUsed == nil || now.Sub(*t.LastUsed) > touchEvery {
		n := now.UTC()
		t.LastUsed = &n
		_ = s.docs.Put(ctx, kindAPIToken, t.ID, t)
	}
	return Principal{Name: u.Username, Role: role, UserID: u.ID}, t, true
}

// dropTokensOf removes every token of a user (the user was deleted or disabled).
func (s *Server) dropTokensOf(ctx context.Context, userID string) {
	ts, err := s.tokensOf(ctx, userID)
	if err != nil {
		return
	}
	for _, t := range ts {
		_ = s.docs.Delete(ctx, kindAPIToken, t.ID)
	}
}
