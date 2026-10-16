// Package web serves the site manager's admin UI and its JSON API, plus the
// registration endpoint used by storage nodes.
package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ibs/internal/config"
	"ibs/internal/release"
	"ibs/internal/site"
	"ibs/internal/tlscert"
	"ibs/internal/version"
)

const (
	sessionCookie = "ibs_session"
	sessionTTL    = 12 * time.Hour
)

type Server struct {
	cfg     config.Config
	site    atomic.Pointer[site.Site]
	onReady func(*site.Site)
	setupMu sync.Mutex
	assets  fs.FS
	build   string // fingerprint of the embedded web interface, so open pages notice an update
	mux     *http.ServeMux

	// TLS manages HTTPS for the web interface and S3.
	TLS *tlscert.Manager
}

// New creates the admin server. s may be nil when the site is not set up
// yet; onReady is called once setup or restore completes.
func New(cfg config.Config, s *site.Site, assets fs.FS, onReady func(*site.Site)) *Server {
	srv := &Server{cfg: cfg, onReady: onReady, assets: assets, mux: http.NewServeMux(), build: buildID(assets)}
	if s != nil {
		srv.site.Store(s)
	}
	srv.routes()
	return srv
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

type handler func(w http.ResponseWriter, r *http.Request, st *site.Site) error

type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func badRequest(msg string) error { return &apiError{http.StatusBadRequest, msg} }

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /api/state", s.handleState)
	m.HandleFunc("POST /api/setup/create", s.handleSetupCreate)
	m.HandleFunc("GET /api/tls/providers", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, tlscert.Providers()) })
	m.HandleFunc("POST /api/setup/restore", s.handleSetupRestore)
	m.HandleFunc("POST /api/login", s.handleLogin)
	m.HandleFunc("POST /api/logout", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		w.WriteHeader(http.StatusNoContent)
	})

	a := s.authed
	m.HandleFunc("GET /api/dashboard", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		d, err := st.Dashboard()
		if err != nil {
			return err
		}
		return writeJSON(w, d)
	}))

	m.HandleFunc("GET /api/events", a(s.handleEvents))

	m.HandleFunc("GET /api/buckets", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		b, err := st.ListBuckets()
		if err != nil {
			return err
		}
		return writeJSON(w, nonNil(b))
	}))
	m.HandleFunc("GET /api/capacity", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		return writeJSON(w, st.StorableCapacity())
	}))
	m.HandleFunc("POST /api/buckets", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		var req struct {
			Name string `json:"name"`
		}
		if err := readJSON(r, &req); err != nil {
			return err
		}
		return st.CreateBucket(strings.TrimSpace(req.Name))
	}))
	m.HandleFunc("DELETE /api/buckets/{name}", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		return st.DeleteBucket(r.PathValue("name"))
	}))
	m.HandleFunc("GET /api/buckets/{name}/objects", a(s.handleListObjects))
	m.HandleFunc("DELETE /api/buckets/{name}/objects", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		return st.DeleteObject(r.PathValue("name"), r.URL.Query().Get("key"))
	}))
	m.HandleFunc("POST /api/buckets/{name}/upload", a(s.handleUpload))
	m.HandleFunc("GET /api/buckets/{name}/download", a(s.handleDownload))

	m.HandleFunc("GET /api/peers", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		p, err := st.Peers()
		if err != nil {
			return err
		}
		return writeJSON(w, nonNil(p))
	}))
	m.HandleFunc("POST /api/peers/invite", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		t, err := st.CreateInvite()
		if err != nil {
			return badRequest(err.Error())
		}
		out := map[string]string{"token": t}
		// The short code too, when a relay of this site supports it.
		if code, err := st.CreateInviteCode(r.Context()); err == nil {
			out["code"] = code
		} else {
			out["code_error"] = err.Error()
		}
		return writeJSON(w, out)
	}))
	m.HandleFunc("POST /api/peers/join", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		var req struct {
			Token string `json:"token"`
		}
		if err := readJSON(r, &req); err != nil {
			return err
		}
		p, err := st.Join(r.Context(), req.Token)
		if err != nil {
			return badRequest(err.Error())
		}
		return writeJSON(w, p)
	}))
	m.HandleFunc("GET /api/relays", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		return writeJSON(w, st.Relays())
	}))
	m.HandleFunc("POST /api/relays", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		var req struct {
			Address string `json:"address"`
			Token   string `json:"token"`
		}
		if err := readJSON(r, &req); err != nil {
			return err
		}
		info, err := st.AddRelay(req.Address, req.Token)
		if err != nil {
			return badRequest(err.Error())
		}
		return writeJSON(w, info)
	}))
	m.HandleFunc("PATCH /api/relays/{id}", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		var req struct {
			Address string `json:"address"`
		}
		if err := readJSON(r, &req); err != nil {
			return err
		}
		if err := st.SetRelayAddress(r.PathValue("id"), req.Address); err != nil {
			return badRequest(err.Error())
		}
		return nil
	}))
	m.HandleFunc("DELETE /api/relays/{id}", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		if err := st.RemoveRelay(r.PathValue("id")); err != nil {
			return badRequest(err.Error())
		}
		return nil
	}))
	m.HandleFunc("GET /api/location", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		return writeJSON(w, map[string]*site.Location{"location": st.Location()})
	}))
	m.HandleFunc("PUT /api/location", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		var pin site.Location
		if err := readJSON(r, &pin); err != nil {
			return err
		}
		l, err := st.SetLocation(&pin)
		if err != nil {
			return badRequest(err.Error())
		}
		return writeJSON(w, map[string]*site.Location{"location": l})
	}))
	m.HandleFunc("DELETE /api/location", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		st.SetLocation(nil)
		w.WriteHeader(http.StatusNoContent)
		return nil
	}))
	m.HandleFunc("GET /api/updates", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		return writeJSON(w, st.Updates())
	}))
	m.HandleFunc("PUT /api/updates/settings", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		var req struct {
			GitHub *bool `json:"github"`
			Betas  *bool `json:"betas"`
			Auto   *bool `json:"auto_install"`
		}
		if err := readJSON(r, &req); err != nil {
			return err
		}
		if req.GitHub != nil {
			st.SetGitHubChecks(*req.GitHub)
		}
		if req.Betas != nil {
			st.SetBetas(*req.Betas)
		}
		if req.Auto != nil {
			st.SetAutoInstall(*req.Auto)
		}
		return writeJSON(w, st.Updates())
	}))
	m.HandleFunc("POST /api/updates/check", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		st.CheckGitHubNow(r.Context())
		return writeJSON(w, st.Updates())
	}))
	m.HandleFunc("POST /api/updates/install", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		var req struct {
			Version string `json:"version"`
		}
		if err := readJSON(r, &req); err != nil {
			return err
		}
		if err := st.Install(req.Version); err != nil {
			return badRequest(err.Error())
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}))
	m.HandleFunc("DELETE /api/updates/versions/{version}", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		if err := st.RemoveVersion(r.PathValue("version")); err != nil {
			return badRequest(err.Error())
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}))
	m.HandleFunc("POST /api/updates/upload", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		r.Body = http.MaxBytesReader(w, r.Body, release.MaxSize)
		f, _, err := r.FormFile("file")
		if err != nil {
			return badRequest("expected an .ibsrelease file")
		}
		defer f.Close()
		in, err := st.UploadRelease(f)
		if err != nil {
			return badRequest(err.Error())
		}
		return writeJSON(w, in)
	}))
	m.HandleFunc("POST /api/updates/keys", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		var req struct {
			Key  string `json:"key"`
			Name string `json:"name"`
		}
		if err := readJSON(r, &req); err != nil {
			return err
		}
		if _, err := st.AddReleaseKey(req.Key, req.Name); err != nil {
			return badRequest(err.Error())
		}
		return writeJSON(w, st.Updates())
	}))
	m.HandleFunc("DELETE /api/updates/keys/{id}", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		if err := st.RemoveReleaseKey(r.PathValue("id")); err != nil {
			return badRequest(err.Error())
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}))
	m.HandleFunc("GET /api/scrub", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		return writeJSON(w, st.ScrubStatus())
	}))
	m.HandleFunc("GET /api/scrub/copies", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		return writeJSON(w, nonNil(st.CopyChecks(r.Context())))
	}))
	for action, f := range map[string]func(*site.Site) error{
		"start":  func(st *site.Site) error { return st.StartScrub(true) },
		"pause":  (*site.Site).PauseScrub,
		"resume": (*site.Site).ResumeScrub,
		"stop":   (*site.Site).StopScrub,
	} {
		m.HandleFunc("POST /api/scrub/"+action, a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
			if err := f(st); err != nil {
				return badRequest(err.Error())
			}
			return writeJSON(w, st.ScrubStatus())
		}))
	}
	m.HandleFunc("PUT /api/scrub/settings", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		var req struct {
			IntervalDays int `json:"interval_days"`
		}
		if err := readJSON(r, &req); err != nil {
			return err
		}
		if err := st.SetScrubInterval(req.IntervalDays); err != nil {
			return badRequest(err.Error())
		}
		return writeJSON(w, st.ScrubStatus())
	}))
	m.HandleFunc("POST /api/ibs/leave", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		if err := st.LeaveIBS(r.Context()); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}))

	m.HandleFunc("GET /api/nodes", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		n, err := st.Nodes()
		if err != nil {
			return err
		}
		return writeJSON(w, nonNil(n))
	}))
	joinToken := func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		token, address := st.NodeJoinToken(host)
		return writeJSON(w, map[string]string{
			"token": token, "address": address, "configured_address": st.Setting("node_address"),
		})
	}
	m.HandleFunc("GET /api/nodes/join-token", a(joinToken))
	m.HandleFunc("POST /api/nodes/join-token", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		if err := st.RotateJoinToken(); err != nil {
			return err
		}
		return joinToken(w, r, st)
	}))
	m.HandleFunc("PUT /api/nodes/address", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		var req struct {
			Address string `json:"address"`
		}
		if err := readJSON(r, &req); err != nil {
			return err
		}
		if err := st.SetNodeAddress(req.Address); err != nil {
			return badRequest(err.Error())
		}
		return joinToken(w, r, st)
	}))
	m.HandleFunc("PUT /api/nodes/{id}/limit", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		// limit: "" = whole disk, "0" = nothing, otherwise a size such as "500G".
		var req struct {
			Limit string `json:"limit"`
		}
		if err := readJSON(r, &req); err != nil {
			return err
		}
		limit, err := config.ParseSize(req.Limit)
		if err != nil {
			return badRequest(err.Error())
		}
		if strings.TrimSpace(req.Limit) != "" && limit == 0 {
			limit = config.NoStorage
		}
		if err := st.SetNodeLimit(r.PathValue("id"), limit); err != nil {
			return badRequest(err.Error())
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}))
	m.HandleFunc("DELETE /api/nodes/{id}", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		if err := st.RemoveNode(r.PathValue("id")); err != nil {
			return badRequest(err.Error())
		}
		return nil
	}))

	m.HandleFunc("GET /api/keys", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		k, err := st.AccessKeys()
		if err != nil {
			return err
		}
		return writeJSON(w, nonNil(k))
	}))
	m.HandleFunc("POST /api/keys", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		var req struct {
			Name string `json:"name"`
		}
		if err := readJSON(r, &req); err != nil {
			return err
		}
		k, err := st.CreateAccessKey(req.Name)
		if err != nil {
			return err
		}
		return writeJSON(w, k)
	}))
	m.HandleFunc("DELETE /api/keys/{id}", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		return st.DeleteAccessKey(r.PathValue("id"))
	}))

	m.HandleFunc("GET /api/settings", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		return writeJSON(w, st.Settings())
	}))
	m.HandleFunc("GET /api/tls", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		return writeJSON(w, s.TLS.Status())
	}))
	m.HandleFunc("PUT /api/tls", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		var in tlscert.Settings
		if err := readJSON(r, &in); err != nil {
			return err
		}
		if in.Require && !s.TLS.Status().Require && r.TLS == nil {
			return badRequest("open the web interface over HTTPS before requiring it, to be sure that address works")
		}
		if err := s.TLS.Update(in); err != nil {
			return badRequest(err.Error())
		}
		return writeJSON(w, s.TLS.Status())
	}))
	m.HandleFunc("PUT /api/settings", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		var in site.Settings
		if err := readJSON(r, &in); err != nil {
			return err
		}
		if err := st.UpdateSettings(in); err != nil {
			return badRequest(err.Error())
		}
		return writeJSON(w, st.Settings())
	}))
	m.HandleFunc("POST /api/password", a(func(w http.ResponseWriter, r *http.Request, st *site.Site) error {
		var req struct {
			Current string `json:"current"`
			New     string `json:"new"`
		}
		if err := readJSON(r, &req); err != nil {
			return err
		}
		if err := st.ChangePassword(req.Current, req.New); err != nil {
			return badRequest(err.Error())
		}
		s.setSession(w, r, st)
		return nil
	}))

	m.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, &apiError{http.StatusNotFound, "not found"})
	})
	m.HandleFunc("/", s.handleStatic)
}

// ---- Plumbing ----

func writeJSON(w http.ResponseWriter, v any) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		return badRequest("invalid JSON body")
	}
	return nil
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func writeErr(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var ae *apiError
	switch {
	case errors.As(err, &ae):
		status = ae.status
	case errors.Is(err, site.ErrNoSuchBucket), errors.Is(err, site.ErrNoSuchKey):
		status = http.StatusNotFound
	case errors.Is(err, site.ErrBucketExists), errors.Is(err, site.ErrBucketNotEmpty):
		status = http.StatusConflict
	case errors.Is(err, site.ErrNoSpace):
		status = http.StatusInsufficientStorage
	case errors.Is(err, site.ErrInvalidBucketName), strings.Contains(err.Error(), "must be"):
		status = http.StatusBadRequest
	}
	if status >= 500 {
		slog.Error("admin api error", "err", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func (s *Server) sign(st *site.Site, expiry string) string {
	m := hmac.New(sha256.New, st.SessionKey())
	m.Write([]byte(expiry))
	return hex.EncodeToString(m.Sum(nil))
}

func (s *Server) setSession(w http.ResponseWriter, r *http.Request, st *site.Site) {
	exp := strconv.FormatInt(time.Now().Add(sessionTTL).Unix(), 10)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: exp + "." + s.sign(st, exp), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds()),
		Secure: r.TLS != nil,
	})
}

func (s *Server) loggedIn(r *http.Request, st *site.Site) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	exp, sig, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	n, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || time.Now().Unix() > n {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(s.sign(st, exp)))
}

// authed requires a ready site and a valid session. State-changing requests
// must also carry the X-IBS header, which cross-site forms cannot set.
func (s *Server) authed(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := s.site.Load()
		if st == nil {
			writeErr(w, &apiError{http.StatusServiceUnavailable, "site is not set up"})
			return
		}
		if !s.loggedIn(r, st) {
			writeErr(w, &apiError{http.StatusUnauthorized, "login required"})
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-IBS") == "" {
			writeErr(w, &apiError{http.StatusForbidden, "missing X-IBS header"})
			return
		}
		st.NoteWebHost(r.Host)
		if err := h(w, r, st); err != nil {
			writeErr(w, err)
		}
	}
}

// ---- Setup and login ----

// startedAt is when this IBS process started; pages use it to tell that IBS
// restarted, however quickly.
var startedAt = time.Now().UnixMilli()

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	st := s.site.Load()
	_, s3Port, _ := strings.Cut(s.cfg.S3Addr, ":")
	out := map[string]any{"setup": st != nil, "authenticated": false, "s3_port": s3Port, "version": version.Version, "build": s.build, "started": startedAt}
	if st != nil {
		out["authenticated"] = s.loggedIn(r, st)
		out["site_name"] = st.Name()
		out["site_id"] = st.ID.SiteID
	}
	writeJSON(w, out)
}

func (s *Server) handleSetupCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SiteName      string            `json:"site_name"`
		Password      string            `json:"password"`
		PeerAdvertise string            `json:"peer_advertise"`
		TLS           *tlscert.Settings `json:"tls"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.TLS != nil {
		req.TLS.Require = false // only after HTTPS has been seen to work
		if err := tlscert.Check(*req.TLS); err != nil {
			writeErr(w, badRequest("HTTPS: "+err.Error()))
			return
		}
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if s.site.Load() != nil {
		writeErr(w, badRequest("site is already set up"))
		return
	}
	cfg := s.cfg
	if req.PeerAdvertise != "" {
		cfg.PeerAdvertise = strings.TrimSpace(req.PeerAdvertise)
	}
	st, phrase, err := site.Create(cfg, strings.TrimSpace(req.SiteName), req.Password)
	if err != nil {
		writeErr(w, badRequest(err.Error()))
		return
	}
	if req.TLS != nil && req.TLS.Enabled() {
		if err := s.TLS.Update(*req.TLS); err != nil {
			slog.Warn("HTTPS settings from setup", "err", err)
		}
	}
	s.site.Store(st)
	s.onReady(st)
	s.setSession(w, r, st)
	writeJSON(w, map[string]string{"phrase": phrase, "site_id": st.ID.SiteID})
}

func (s *Server) handleSetupRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phrase        string `json:"phrase"`
		NodeAddress   string `json:"node_address"`
		PeerAddress   string `json:"peer_address"`
		RelayAddress  string `json:"relay_address"`
		RelayToken    string `json:"relay_token"`
		PeerSiteID    string `json:"peer_site_id"`
		Password      string `json:"password"`
		PeerAdvertise string `json:"peer_advertise"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if s.site.Load() != nil {
		writeErr(w, badRequest("site is already set up"))
		return
	}
	cfg := s.cfg
	if req.PeerAdvertise != "" {
		cfg.PeerAdvertise = strings.TrimSpace(req.PeerAdvertise)
	}
	// No total time limit: the downloads fail when they make no progress.
	st, err := site.Restore(r.Context(), cfg, site.RestoreOptions{
		Phrase: req.Phrase, NodeAddress: strings.TrimSpace(req.NodeAddress), PeerAddress: strings.TrimSpace(req.PeerAddress), RelayAddress: req.RelayAddress, RelayToken: strings.TrimSpace(req.RelayToken),
		PeerSiteID: strings.TrimSpace(req.PeerSiteID), Password: req.Password,
	})
	if err != nil {
		writeErr(w, badRequest(err.Error()))
		return
	}
	s.site.Store(st)
	s.onReady(st)
	s.setSession(w, r, st)
	writeJSON(w, map[string]string{"site_id": st.ID.SiteID, "site_name": st.Name()})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	st := s.site.Load()
	if st == nil {
		writeErr(w, &apiError{http.StatusServiceUnavailable, "site is not set up"})
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if !st.CheckAdminPassword(req.Password) {
		time.Sleep(500 * time.Millisecond)
		writeErr(w, &apiError{http.StatusUnauthorized, "wrong password"})
		return
	}
	s.setSession(w, r, st)
	w.WriteHeader(http.StatusNoContent)
}

// handleEvents streams change notifications to the UI as Server-Sent
// Events. Each event names the topics that changed; the UI refetches only
// what it shows. A comment line every 25s keeps proxies from closing the
// stream, and the stream ends when the session expires.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, st *site.Site) error {
	fl, ok := w.(http.Flusher)
	if !ok {
		return errors.New("streaming not supported")
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, "retry: 2000\n\n")
	fl.Flush()
	ch := st.Subscribe(r.Context())
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return nil
		case topics := <-ch:
			b, _ := json.Marshal(topics)
			io.WriteString(w, "event: change\ndata: "+string(b)+"\n\n")
		case <-ping.C:
			if !s.loggedIn(r, st) {
				return nil
			}
			io.WriteString(w, ": ping\n\n")
		}
		fl.Flush()
	}
}

// ---- Objects via the UI ----

func (s *Server) handleListObjects(w http.ResponseWriter, r *http.Request, st *site.Site) error {
	q := r.URL.Query()
	res, err := st.ListObjects(r.PathValue("name"), q.Get("prefix"), "/", q.Get("marker"), 500)
	if err != nil {
		return err
	}
	return writeJSON(w, map[string]any{
		"objects":   nonNil(res.Objects),
		"prefixes":  nonNil(res.CommonPrefixes),
		"truncated": res.Truncated,
		"next":      res.NextMarker,
	})
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request, st *site.Site) error {
	bucket := r.PathValue("name")
	prefix := r.URL.Query().Get("prefix")
	mr, err := r.MultipartReader()
	if err != nil {
		return badRequest("expected multipart form")
	}
	var uploaded []string
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return badRequest(err.Error())
		}
		if part.FileName() == "" {
			continue
		}
		key := prefix + path.Base(part.FileName())
		ct := part.Header.Get("Content-Type")
		if ct == "" || ct == "application/octet-stream" {
			if t := mime.TypeByExtension(path.Ext(key)); t != "" {
				ct = t
			}
		}
		if _, err := st.PutObject(r.Context(), bucket, key, part, ct, nil); err != nil {
			return err
		}
		uploaded = append(uploaded, key)
	}
	return writeJSON(w, map[string]any{"uploaded": nonNil(uploaded)})
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request, st *site.Site) error {
	o, err := st.HeadObject(r.PathValue("name"), r.URL.Query().Get("key"))
	if err != nil {
		return err
	}
	// Read the first block before committing to a status, so a block that
	// can't be read (or repaired) is reported as an error, not an empty file.
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(st.WriteObject(r.Context(), pw, o, 0, o.Size)) }()
	defer pr.Close()
	first := make([]byte, 32<<10)
	n, err := io.ReadAtLeast(pr, first, min(int(o.Size), 1))
	if err != nil && !(o.Size == 0 && errors.Is(err, io.EOF)) && n == 0 {
		return err
	}
	w.Header().Set("Content-Type", o.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(o.Size, 10))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(o.Key)}))
	w.Write(first[:n])
	if _, err := io.Copy(w, pr); err != nil {
		slog.Warn("download interrupted", "key", o.Key, "err", err)
	}
	return nil
}

// ---- Static UI ----

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if s.assets == nil {
		http.Error(w, "UI not built", http.StatusNotFound)
		return
	}
	p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if p == "" {
		p = "index.html"
	}
	if _, err := fs.Stat(s.assets, p); err != nil {
		p = "index.html" // single-page app routes
	}
	data, err := fs.ReadFile(s.assets, p)
	if err != nil {
		http.Error(w, "UI not built: run `npm run build` in web/", http.StatusNotFound)
		return
	}
	if ct := mime.TypeByExtension(path.Ext(p)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if strings.HasPrefix(p, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.Write(data)
}

// buildID fingerprints the web interface: its index.html names the hashed
// asset files, so it changes with every build, released or not.
func buildID(assets fs.FS) string {
	if assets == nil {
		return version.Version
	}
	b, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		return version.Version
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:6])
}
