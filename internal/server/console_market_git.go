package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/forgelinehq/forgeline/internal/docstore"
)

// A signed git source for marketplace packages (note 019). The operator names a repository and the
// ed25519 keys they trust; the server reads manifests out of the repository's object store without
// checking anything out, and lists a package as installable only if a trusted key signed its content
// hash. Nothing from the repository is ever executed.

const (
	signedMessagePrefix = "forgeline-market-v1:"
	kindMarketSeen      = "marketseen"
	maxGitFiles         = 400
	maxGitFileBytes     = 256 << 10
	maxGitTotalBytes    = 16 << 20 // all manifests and signatures together, in memory
	gitFetchTimeout     = 2 * time.Minute
	gitRefreshEvery     = time.Hour
)

var (
	gitRefRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,99}$`)
	keyIDRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,38}$`)
	gitFileRE   = regexp.MustCompile(`^packages/([a-z0-9][a-z0-9._-]{0,60})\.(json|sig)$`)
	gitTreeLine = regexp.MustCompile(`^(\d{6}) (\w+) ([0-9a-f]{40,64}) +(\d+|-)$`)
)

// maxGitRepoBytes bounds what the bare repository may take on disk; a var so a test can lower it.
var maxGitRepoBytes int64 = 128 << 20

// marketGitConfig is what the operator set.
type marketGitConfig struct {
	URL  string
	Ref  string
	Keys map[string]ed25519.PublicKey
	Err  string // why the configuration is unusable, when it is
}

func (c marketGitConfig) configured() bool { return c.URL != "" }

// marketGitFromEnv reads the operator's configuration. A bad value disables the source and says why,
// rather than trusting half of a configuration.
func marketGitFromEnv(getenv func(string) string) marketGitConfig {
	c := marketGitConfig{URL: strings.TrimSpace(getenv("FORGELINE_MARKET_GIT")), Ref: strings.TrimSpace(getenv("FORGELINE_MARKET_GIT_REF"))}
	if c.URL == "" {
		return c
	}
	if c.Ref == "" {
		c.Ref = "main"
	}
	keys := getenv("FORGELINE_MARKET_KEYS")
	if f := strings.TrimSpace(getenv("FORGELINE_MARKET_KEYS_FILE")); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			c.Err = "FORGELINE_MARKET_KEYS_FILE cannot be read"
			return c
		}
		keys += "\n" + string(b)
	}
	c.Keys, c.Err = parseTrustedKeys(keys)
	if c.Err == "" {
		c.Err = checkGitSource(c.URL, c.Ref)
	}
	if c.Err == "" && len(c.Keys) == 0 {
		c.Err = "no trusted keys are set (FORGELINE_MARKET_KEYS), so nothing from git could be trusted"
	}
	return c
}

func parseTrustedKeys(raw string) (map[string]ed25519.PublicKey, string) {
	keys := map[string]ed25519.PublicKey{}
	var lines []string
	for _, l := range strings.Split(raw, "\n") {
		l, _, _ = strings.Cut(l, "#")
		lines = append(lines, l)
	}
	for _, f := range strings.FieldsFunc(strings.Join(lines, "\n"), func(r rune) bool { return r == ',' || r == ';' || r == '\n' || r == '\r' || r == ' ' || r == '\t' }) {
		id, b64, ok := strings.Cut(f, "=")
		if !ok || !keyIDRE.MatchString(id) {
			return nil, fmt.Sprintf("a trusted key must look like id=base64 with a lowercase id, got %q", truncate(f, 20))
		}
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Sprintf("trusted key %q is not a base64 ed25519 public key (32 bytes)", id)
		}
		if _, dup := keys[id]; dup {
			return nil, fmt.Sprintf("trusted key id %q is listed twice", id)
		}
		keys[id] = ed25519.PublicKey(raw)
	}
	return keys, ""
}

// checkGitSource accepts https and file URLs and plain absolute paths, and refuses credentials in the
// URL, anything that could be read as an option, and unusual refs.
func checkGitSource(rawURL, ref string) string {
	if !gitRefRE.MatchString(ref) || strings.Contains(ref, "..") {
		return "FORGELINE_MARKET_GIT_REF is not a plain branch or tag name"
	}
	if strings.ContainsAny(rawURL, " \t\r\n\x00") {
		return "FORGELINE_MARKET_GIT is not a plain URL or path"
	}
	if strings.HasPrefix(rawURL, "/") {
		return ""
	}
	u, err := url.Parse(rawURL)
	switch {
	case err != nil || (u.Scheme != "https" && u.Scheme != "file"):
		return "FORGELINE_MARKET_GIT must be an https:// or file:// URL or an absolute path"
	case u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(rawURL, "?#"):
		return "FORGELINE_MARKET_GIT must not have a query or fragment"
	case u.User != nil:
		return "FORGELINE_MARKET_GIT must not contain credentials; the repository has to be readable without them"
	case u.Scheme == "https" && u.Host == "":
		return "FORGELINE_MARKET_GIT has no host"
	}
	return ""
}

// displayURL is what the console may show: an https URL without query or fragment, and only the last
// element of a local path, so a server's directory layout is not given to every developer.
func (c marketGitConfig) displayURL() string {
	if strings.HasPrefix(c.URL, "/") {
		return "local repository " + filepath.Base(c.URL)
	}
	u, err := url.Parse(c.URL)
	if err != nil {
		return "a git repository"
	}
	if u.Scheme == "file" {
		return "local repository " + filepath.Base(u.Path)
	}
	return u.Scheme + "://" + u.Host + u.Path
}

// marketSnapshot is the last verified-readable state of the repository.
type marketSnapshot struct {
	Files     map[string][]byte // "name.json" and "name.sig" under packages/
	Large     map[string]bool   // files that were too large to read
	Commit    string
	FetchedAt time.Time
	Err       string
}

type marketGit struct {
	mu   sync.Mutex
	cfg  *marketGitConfig // set by tests; nil means the environment
	snap marketSnapshot
	busy bool

	entries   []marketEntry // verified snapshot, for entriesFP
	entriesFP string
	env       marketGitConfig // the environment's configuration, re-read every few seconds
	envAt     time.Time
}

const envConfigTTL = 5 * time.Second

func (s *Server) marketGitConfig() marketGitConfig {
	s.market.mu.Lock()
	if s.market.cfg != nil {
		c := *s.market.cfg
		s.market.mu.Unlock()
		return c
	}
	if time.Since(s.market.envAt) < envConfigTTL {
		c := s.market.env
		s.market.mu.Unlock()
		return c
	}
	s.market.mu.Unlock()
	c := marketGitFromEnv(os.Getenv) // reads the key file, so not under the lock
	s.market.mu.Lock()
	s.market.env, s.market.envAt = c, time.Now()
	s.market.mu.Unlock()
	return c
}

// gitEnviron is git's whole environment: PATH, a home that is the bare repository, no configuration
// files, no prompts and only the two transports, plus the operator's proxy and certificate settings
// (theirs to set). Nothing else is passed on, so no secret in the server's environment can reach git.
func gitEnviron(dir string, getenv func(string) string) []string {
	env := []string{"PATH=" + getenv("PATH"), "HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=https:file", "GIT_ASKPASS=/bin/false", "LC_ALL=C"}
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR", "GIT_SSL_CAINFO"} {
		if v := getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// gitRun runs git with nothing but PATH in its environment: no configuration files, no prompts, no
// helpers, and only the two transports the source may use.
func gitRun(ctx context.Context, dir string, stdin []byte, args ...string) ([]byte, error) {
	full := append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "protocol.ext.allow=never",
		"-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "-c", "protocol.file.allow=always", "--git-dir", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = gitEnviron(dir, os.Getenv)
	cmd.WaitDelay = 5 * time.Second // a child that keeps the pipes open must not outlive the timeout
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb limitBuf
	out.max, errb.max = 1<<20, 4<<10
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(truncate(errb.String(), 200)))
	}
	return out.Bytes(), nil
}

// limitBuf is a bytes.Buffer that stops growing at max.
type limitBuf struct {
	bytes.Buffer
	max int
}

func (b *limitBuf) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// readGitSnapshot fetches the ref into a bare repository under dir and reads packages/ out of it.
func readGitSnapshot(ctx context.Context, cfg marketGitConfig, dir string) (marketSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, gitFetchTimeout)
	defer cancel()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return marketSnapshot{}, err
	}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		if _, err := gitRun(ctx, dir, nil, "init", "--bare", "-q", "--template="); err != nil {
			return marketSnapshot{}, err
		}
	}
	// Objects from earlier fetches are unreachable (no ref is kept), so drop them before fetching.
	_, _ = gitRun(ctx, dir, nil, "gc", "-q", "--prune=now")
	if _, err := gitRun(ctx, dir, nil, "fetch", "-q", "--depth", "1", "--no-tags", "--no-recurse-submodules", "--", cfg.URL, cfg.Ref); err != nil {
		return marketSnapshot{}, err
	}
	if n, err := dirSize(dir); err != nil || n > maxGitRepoBytes {
		os.RemoveAll(dir) // start clean next time rather than keep a repository that large
		return marketSnapshot{}, fmt.Errorf("the repository is larger than %d MB", maxGitRepoBytes>>20)
	}
	rev, err := gitRun(ctx, dir, nil, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil {
		return marketSnapshot{}, err
	}
	commit := strings.TrimSpace(string(rev))
	if len(commit) < 40 {
		return marketSnapshot{}, errors.New("the fetched ref is not a commit")
	}
	tree, err := gitRun(ctx, dir, nil, "ls-tree", "-z", "--long", commit, "--", "packages/")
	if err != nil {
		return marketSnapshot{}, err
	}
	snap := marketSnapshot{Files: map[string][]byte{}, Large: map[string]bool{}, Commit: commit, FetchedAt: time.Now().UTC()}
	entries := strings.Split(strings.TrimRight(string(tree), "\x00"), "\x00")
	if len(entries) > maxGitFiles*2 {
		return marketSnapshot{}, fmt.Errorf("packages/ holds more than %d files", maxGitFiles*2)
	}
	total := 0
	for _, e := range entries {
		meta, name, ok := strings.Cut(e, "\t")
		if !ok {
			continue
		}
		m := gitTreeLine.FindStringSubmatch(meta)
		// Regular files only: a symlink (120000), submodule (160000) or directory is skipped.
		if m == nil || m[2] != "blob" || (m[1] != "100644" && m[1] != "100755") || !gitFileRE.MatchString(name) {
			continue
		}
		if size, _ := strconv.Atoi(m[4]); size > maxGitFileBytes {
			snap.Large[strings.TrimPrefix(name, "packages/")] = true
			continue
		}
		blob, err := gitRun(ctx, dir, nil, "cat-file", "blob", m[3])
		if err != nil {
			return marketSnapshot{}, err
		}
		if total += len(blob); total > maxGitTotalBytes {
			return marketSnapshot{}, fmt.Errorf("packages/ holds more than %d MB", maxGitTotalBytes>>20)
		}
		snap.Files[strings.TrimPrefix(name, "packages/")] = append([]byte{}, blob...) // never nil, so an empty file is not a missing one
	}
	return snap, nil
}

func dirSize(dir string) (int64, error) {
	var n int64
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if info, ierr := d.Info(); ierr == nil && !d.IsDir() {
			n += info.Size()
		}
		return nil
	})
	return n, err
}

// refreshMarketGit fetches the repository and keeps the result, or keeps the last good one and says
// why it failed. One refresh runs at a time.
func (s *Server) refreshMarketGit(ctx context.Context) error {
	cfg := s.marketGitConfig()
	if !cfg.configured() {
		return errors.New("no git source is configured")
	}
	if cfg.Err != "" {
		return errors.New(cfg.Err)
	}
	s.market.mu.Lock()
	if s.market.busy {
		s.market.mu.Unlock()
		return errors.New("a refresh is already running")
	}
	s.market.busy = true
	s.market.mu.Unlock()
	defer func() { s.market.mu.Lock(); s.market.busy = false; s.market.mu.Unlock() }()

	snap, err := readGitSnapshot(ctx, cfg, filepath.Join(s.cfg.DataDir, "market-git"))
	s.market.mu.Lock()
	if err != nil {
		s.market.snap.Err = err.Error()
		s.market.mu.Unlock()
		return err
	}
	s.market.snap = snap
	s.market.mu.Unlock()
	s.recordSeen(ctx, cfg, snap)
	return nil
}

// signedFile is the content of a .sig file.
type signedFile struct {
	KeyID     string `json:"key_id"`
	Signature string `json:"signature"`
}

// signedMessage is what the signature covers.
func signedMessage(hash string) []byte { return []byte(signedMessagePrefix + hash) }

// verifySigned parses a manifest strictly and checks that a trusted key signed its content hash.
func verifySigned(manifest, sig []byte, keys map[string]ed25519.PublicKey) (MarketPackage, string, error) {
	var p MarketPackage
	dec := json.NewDecoder(bytes.NewReader(manifest))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, "", fmt.Errorf("the manifest is not valid: %w", err)
	}
	if dec.More() {
		return p, "", errors.New("the manifest has trailing content")
	}
	if sig == nil {
		return p, "", errors.New("it is not signed: there is no .sig file beside it")
	}
	var sf signedFile
	sdec := json.NewDecoder(bytes.NewReader(sig))
	sdec.DisallowUnknownFields()
	if err := sdec.Decode(&sf); err != nil || sf.KeyID == "" || sf.Signature == "" {
		return p, "", errors.New("the .sig file is not {\"key_id\", \"signature\"}")
	}
	pub, ok := keys[sf.KeyID]
	if !ok {
		return p, sf.KeyID, fmt.Errorf("it is signed with key %q, which this server does not trust", sf.KeyID)
	}
	raw, err := base64.StdEncoding.DecodeString(sf.Signature)
	if err != nil || len(raw) != ed25519.SignatureSize {
		return p, sf.KeyID, errors.New("the signature is not a base64 ed25519 signature")
	}
	// The hash is recomputed from the parsed content, never taken from the file, so a signature
	// cannot vouch for content other than what would be installed.
	if !ed25519.Verify(pub, signedMessage(p.contentHash()), raw) {
		return p, sf.KeyID, fmt.Errorf("the signature does not match the content (key %q)", sf.KeyID)
	}
	return p, sf.KeyID, nil
}

// SignManifest fills in the manifest's sha256 and returns it with a signature file, for publishers.
func SignManifest(manifest []byte, keyID string, priv ed25519.PrivateKey) (signed, sig []byte, err error) {
	var p MarketPackage
	dec := json.NewDecoder(bytes.NewReader(manifest))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, nil, fmt.Errorf("the manifest is not valid: %w", err)
	}
	if !keyIDRE.MatchString(keyID) {
		return nil, nil, errors.New("the key id must be a lowercase slug")
	}
	p.SHA256 = p.contentHash()
	if err := p.check(); err != nil {
		return nil, nil, err
	}
	signed, _ = json.MarshalIndent(p, "", "  ")
	sig, _ = json.MarshalIndent(signedFile{KeyID: keyID, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, signedMessage(p.SHA256)))}, "", "  ")
	return append(signed, '\n'), append(sig, '\n'), nil
}

// --- rollback and replay ---

// marketSeen is the highest version of a package this server has verified from git.
type marketSeen struct {
	ID      string            `json:"id"`
	Version int               `json:"version"`
	Hashes  map[string]string `json:"hashes"` // version to hash, for versions seen
}

// recordSeen remembers the versions of packages that verified, so an older one cannot be served later.
// A record it cannot read is left alone rather than rebuilt, so a store error never resets it.
func (s *Server) recordSeen(ctx context.Context, cfg marketGitConfig, snap marketSnapshot) {
	for name, blob := range snap.Files {
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		p, _, err := verifySigned(blob, snap.Files[strings.TrimSuffix(name, ".json")+".sig"], cfg.Keys)
		if err != nil || p.check() != nil {
			continue
		}
		var seen marketSeen
		switch err := s.docs.Get(ctx, kindMarketSeen, p.ID, &seen); {
		case errors.Is(err, docstore.ErrNotFound):
			seen = marketSeen{ID: p.ID}
		case err != nil:
			continue
		}
		if seen.Hashes == nil {
			seen.Hashes = map[string]string{}
		}
		v := fmt.Sprint(p.Version)
		if old, ok := seen.Hashes[v]; ok && old != p.SHA256 {
			continue // equivocation: the first one seen stays the record
		}
		if p.Version < seen.Version || (seen.Hashes[v] == p.SHA256 && seen.Version >= p.Version) {
			continue // nothing new
		}
		seen.Version, seen.Hashes[v] = p.Version, p.SHA256
		_ = s.docs.Put(ctx, kindMarketSeen, p.ID, seen)
	}
}

// gitEntries turns the snapshot into catalogue entries. Every manifest appears; those that fail
// verification carry the reason and cannot be installed. Parsing and verifying is done once per
// snapshot and key set; only the rollback check reads the store on each call.
func (s *Server) gitEntries(ctx context.Context) []marketEntry {
	cfg := s.marketGitConfig()
	if !cfg.configured() || cfg.Err != "" {
		return nil
	}
	s.market.mu.Lock()
	snap := s.market.snap
	fp := fmt.Sprintf("%s|%s|%s", snap.Commit, snap.FetchedAt.Format(time.RFC3339Nano), keyFingerprint(cfg.Keys))
	cached, ok := s.market.entries, s.market.entriesFP == fp
	s.market.mu.Unlock()
	if !ok {
		cached = verifySnapshot(snap, cfg.Keys)
		s.market.mu.Lock()
		s.market.entries, s.market.entriesFP = cached, fp
		s.market.mu.Unlock()
	}
	out := make([]marketEntry, len(cached))
	copy(out, cached)
	for i := range out {
		if out[i].Problem == "" {
			if err := s.rollbackCheck(ctx, out[i].MarketPackage); err != nil {
				out[i].Problem = err.Error()
			}
		}
	}
	return out
}

func keyFingerprint(keys map[string]ed25519.PublicKey) string {
	var ids []string
	for id, k := range keys {
		ids = append(ids, id+"="+base64.StdEncoding.EncodeToString(k))
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// verifySnapshot parses and verifies every manifest in a snapshot.
func verifySnapshot(snap marketSnapshot, keys map[string]ed25519.PublicKey) []marketEntry {
	var names []string
	for n := range snap.Files {
		if strings.HasSuffix(n, ".json") {
			names = append(names, n)
		}
	}
	for n := range snap.Large {
		if strings.HasSuffix(n, ".json") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var out []marketEntry
	for _, n := range names {
		base := strings.TrimSuffix(n, ".json")
		e := marketEntry{Source: "git"}
		var err error
		switch {
		case snap.Large[n]:
			err = fmt.Errorf("the manifest is larger than %d KB", maxGitFileBytes>>10)
		case snap.Large[base+".sig"]:
			err = fmt.Errorf("the signature file is larger than %d KB", maxGitFileBytes>>10)
		default:
			sig := snap.Files[base+".sig"] // nil only when there is no file; an empty file is non-nil
			var p MarketPackage
			p, e.Signer, err = verifySigned(snap.Files[n], sig, keys)
			e.MarketPackage = p
			if err == nil {
				err = p.check()
			}
		}
		if err != nil {
			e.Problem = err.Error()
			if e.ID == "" || !marketIDRE.MatchString(e.ID) {
				e.ID = base
			}
		}
		for _, a := range e.Agents {
			if a.Tier == tierReview {
				e.Writes = true
			}
		}
		out = append(out, e)
	}
	return out
}

func (s *Server) rollbackCheck(ctx context.Context, p MarketPackage) error {
	var seen marketSeen
	switch err := s.docs.Get(ctx, kindMarketSeen, p.ID, &seen); {
	case errors.Is(err, docstore.ErrNotFound):
		return nil
	case err != nil:
		return errors.New("the record of versions seen could not be read, so a rollback cannot be ruled out; try again")
	}
	if p.Version < seen.Version {
		return fmt.Errorf("version %d is older than version %d, which this server has already seen signed; a rollback is refused", p.Version, seen.Version)
	}
	if h, ok := seen.Hashes[fmt.Sprint(p.Version)]; ok && h != p.SHA256 {
		return fmt.Errorf("a different manifest was signed for version %d before; the first one seen stands", p.Version)
	}
	return nil
}

// handleMarketForget clears what this server remembers of a package's signed versions, for when a
// publisher signed a wrong version number or a key was compromised and has been replaced. Without it
// a mistaken version 999 would lock the publisher out for good.
func (s *Server) handleMarketForget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.docs.Delete(r.Context(), kindMarketSeen, id)
	if err != nil && !errors.Is(err, docstore.ErrNotFound) {
		s.internalError(w, "clearing the record", err)
		return
	}
	s.auditReq(r, "market.forget", "market/"+id, "versions seen from git forgotten", true)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- API ---

// marketGitStatus is what the catalogue says about the git source.
func (s *Server) marketGitStatus() map[string]any {
	cfg := s.marketGitConfig()
	if !cfg.configured() {
		return map[string]any{"configured": false}
	}
	s.market.mu.Lock()
	snap := s.market.snap
	s.market.mu.Unlock()
	var ids []string
	for id := range cfg.Keys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	st := map[string]any{"configured": true, "url": cfg.displayURL(), "ref": cfg.Ref, "keys": ids}
	switch {
	case cfg.Err != "":
		st["error"] = cfg.Err
	default:
		if snap.Commit != "" {
			st["commit"], st["fetched_at"] = snap.Commit[:12], snap.FetchedAt
		}
		if snap.Err != "" {
			st["error"] = snap.Err
		}
	}
	return st
}

func (s *Server) handleMarketRefresh(w http.ResponseWriter, r *http.Request) {
	err := s.refreshMarketGit(r.Context())
	s.auditReq(r, "market.refresh", "market/git", refreshNote(err), err == nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.marketGitStatus())
}

func refreshNote(err error) string {
	if err == nil {
		return "fetched"
	}
	return truncate(err.Error(), 200)
}

// startMarketGit fetches at start and then every hour, when a source is configured.
func (s *Server) startMarketGit(ctx context.Context) {
	if !s.marketGitConfig().configured() {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			if err := s.refreshMarketGit(ctx); err != nil {
				s.log.Warn("refreshing the marketplace git source", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(gitRefreshEvery):
			}
		}
	}()
}
