package site

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"ibs/internal/db"
	"ibs/internal/release"
	"ibs/internal/stall"
	"ibs/internal/version"
)

// Updates: IBS versions are signed releases kept in the data directory (see
// internal/release) and run by the launcher. New versions come from GitHub
// (if enabled), from other sites in the IBS that hold a version signed by a
// key this site trusts, or from an upload. Nothing is ever installed on
// another site: sites only tell each other what they hold. Storage nodes
// follow their manager's version, one node at a time.

// RestartRequested is closed when IBS should exit so the launcher starts the
// version now marked current (after an install or a rollback).
var RestartRequested = make(chan struct{})

var restartOnce sync.Once

// RestartCode is the exit code that asks the launcher for a restart.
const RestartCode = 75

// UnderLauncher reports whether IBS was started by the launcher, which is
// what makes installing versions possible.
func UnderLauncher() bool { return os.Getenv("IBS_LAUNCHER") != "" }

// LauncherVersion is the version of the launcher that started IBS ("" without
// one). The launcher is a program of its own, with its own versions.
func LauncherVersion() string { return os.Getenv("IBS_LAUNCHER") }

// envKey reports whether a key is set with IBS_TRUSTED_KEYS; the launcher adds
// those on every start, so they are changed there, not here.
func envKey(id string) bool {
	keys, _ := release.ParseKeyList(os.Getenv("IBS_TRUSTED_KEYS"))
	for _, k := range keys {
		if k.ID == id {
			return true
		}
	}
	return false
}

// ReleaseRef is what a site tells others about a version it holds.
type ReleaseRef struct {
	Version       string `json:"version"`
	Platform      string `json:"platform"`
	KeyID         string `json:"key_id"`
	MinCompatible string `json:"min_compatible,omitempty"`
	DBLevel       int    `json:"db_level,omitempty"`
}

type updateState struct {
	mu            sync.Mutex
	githubChecked time.Time
	githubRetry   time.Time // GitHub's limit is used up until then
	githubError   string
	githubLatest  string
	fetchRun      sync.Mutex        // one fetchReleases at a time
	fetching      map[string]bool   // versions being fetched
	announced     map[string]bool   // new versions on other sites already logged
	fetchErrors   map[string]string // version -> why the last fetch from a site failed
	nodeUpdate    struct {          // the one storage node currently updating
		id    string
		since time.Time
	}
}

// CheckIntervals are how often GitHub can be asked for a new release, in
// minutes: every 15 minutes, 6 hours, or a day (the default).
var CheckIntervals = []int{15, 6 * 60, 24 * 60}

// CheckInterval is how often GitHub is asked for a new release.
func (s *Site) CheckInterval() time.Duration {
	m := int(s.SettingInt("updates_interval"))
	if !slices.Contains(CheckIntervals, m) {
		m = 24 * 60 // once a day unless set
	}
	return time.Duration(m) * time.Minute
}

// SetCheckInterval sets how often GitHub is asked, in minutes (CheckIntervals).
func (s *Site) SetCheckInterval(minutes int) error {
	if !slices.Contains(CheckIntervals, minutes) {
		return errors.New("check every 15 minutes, 6 hours or day")
	}
	return s.SetSetting("updates_interval", strconv.Itoa(minutes))
}

func (s *Site) store() release.Store { return release.Store{Dir: s.cfg.DataDir} }

// GitHubChecks reports whether this site looks for new versions on GitHub.
func (s *Site) GitHubChecks() bool { return s.Setting("updates_github") != "0" }

// Betas reports whether betas count as new versions: the Updates page's
// setting, or IBS_BETA until it is changed there.
func (s *Site) Betas() bool {
	switch s.Setting("updates_betas") {
	case "1":
		return true
	case "0":
		return false
	}
	return EnvBeta()
}

// EnvBeta reports whether IBS_BETA is set to true (the launcher's flag).
func EnvBeta() bool {
	b, _ := strconv.ParseBool(os.Getenv("IBS_BETA"))
	return b
}

// SetBetas sets whether betas count as new versions.
func (s *Site) SetBetas(on bool) {
	v := "0"
	if on {
		v = "1"
	}
	db.SetSetting(s.DB, "updates_betas", v)
	if on && s.GitHubChecks() {
		go s.CheckGitHubNow(context.Background())
	}
}

// wanted reports whether v counts as a new version: stable releases always,
// betas only when Betas is on.
func (s *Site) wanted(v string) bool { return s.Betas() || !release.Prerelease(v) }

// releaseRefs lists the versions this site holds, for its status.
func (s *Site) releaseRefs() []ReleaseRef {
	var out []ReleaseRef
	for _, in := range s.store().List(s.store().Keys()) {
		if !in.Bad {
			out = append(out, ReleaseRef{in.Version, in.Platform, in.KeyID, in.MinCompatible, in.DBLevel})
		}
	}
	return out
}

// dbLevel is the highest database level that has used this site's database.
func (s *Site) dbLevel() int {
	return max(int(s.SettingInt("db_level")), db.Level)
}

// dbBlocker says why m can never run on this site's database again: a newer
// version changed it in a way m doesn't know; "" when m can.
func (s *Site) dbBlocker(m release.Manifest) string {
	if why := db.BreakingSince(m.DBLevel, s.dbLevel()); why != "" {
		return "a newer version changed this site's database for " + why
	}
	return ""
}

// removeUnrunnable removes held versions that can never run here again
// (dbBlocker). Only once the running version passed its probation: until
// then the launcher may still need to go back to the one before.
func (s *Site) removeUnrunnable() {
	st := s.store()
	if !UnderLauncher() || st.Good() != version.Version {
		return
	}
	for _, in := range st.List(st.Keys()) {
		if in.Version == version.Version || release.Compare(in.Version, version.Version) > 0 {
			continue
		}
		if why := s.dbBlocker(in.Manifest); why != "" && st.Remove(in.Version) == nil {
			s.log.Info("removed a version this site can't go back to", "version", in.Version, "why", why)
			s.notify(TopicPeers)
		}
	}
}

// compatBlocker explains why this site can't run m, or returns "".
func (s *Site) compatBlocker(m release.Manifest) string {
	if why := s.dbBlocker(m); why != "" {
		return why
	}
	sites, _ := s.Network()
	for _, n := range sites {
		if !release.Compatible(m.Version, m.MinCompatible, n.Version, n.MinCompatible) {
			return fmt.Sprintf("it doesn't work with %s, which runs %s", n.Name, n.Version)
		}
	}
	return ""
}

// VersionInfo is one installed version, for the Updates page.
type VersionInfo struct {
	Version  string   `json:"version"`
	KeyID    string   `json:"key_id"`
	KeyName  string   `json:"key_name"`
	Released int64    `json:"released"`
	Current  bool     `json:"current"` // the one running
	Bad      bool     `json:"bad"`     // failed to start before
	Newer    bool     `json:"newer"`   // newer than the one running
	Beta     bool     `json:"beta"`
	Blocker  string   `json:"blocker"` // why it can't be run here, if so
	Notes    string   `json:"notes"`   // what changed (Markdown), from the release or GitHub
	Sites    []string `json:"sites"`   // other sites that run it
}

// SeenVersion is a version another site runs that this site doesn't hold.
type SeenVersion struct {
	Version string   `json:"version"`
	Sites   []string `json:"sites"`
	Trusted bool     `json:"trusted"`         // signed with a key this site trusts; it will be fetched
	Error   string   `json:"error,omitempty"` // why the last fetch failed
}

// KeyInfo is a trusted release key.
type KeyInfo struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Key     string   `json:"key"`
	Default bool     `json:"default"`
	Env     bool     `json:"env"`    // set with IBS_TRUSTED_KEYS
	Signed  []string `json:"signed"` // installed versions signed with it
}

// UpdatesInfo is everything the Updates page shows.
type UpdatesInfo struct {
	Running         string        `json:"running"`
	Launcher        bool          `json:"launcher"` // installing is possible
	LauncherVersion string        `json:"launcher_version,omitempty"`
	GitHub          bool          `json:"github"`
	GitHubChecked   int64         `json:"github_checked,omitempty"`
	Old             OldVersions   `json:"old"`
	GitHubError     string        `json:"github_error,omitempty"`
	GitHubRetry     int64         `json:"github_retry,omitempty"` // GitHub's limit is used up until then
	Versions        []VersionInfo `json:"versions"`
	Seen            []SeenVersion `json:"seen"`
	Keys            []KeyInfo     `json:"keys"`
	Betas           bool          `json:"betas"` // betas count as new versions
	AutoInstall     bool          `json:"auto_install"`
	CheckMinutes    int           `json:"check_minutes"` // how often GitHub is asked
	Newer           string        `json:"newer"`         // newest version newer than the running one, held or seen; "" when up to date
}

func (s *Site) Updates() UpdatesInfo {
	keys := s.store().Keys()
	names := map[string]string{}
	info := UpdatesInfo{Running: version.Version, Launcher: UnderLauncher(), LauncherVersion: LauncherVersion(), GitHub: s.GitHubChecks(), Betas: s.Betas(), AutoInstall: s.AutoInstall(), CheckMinutes: int(s.CheckInterval().Minutes()), Versions: []VersionInfo{}, Seen: []SeenVersion{}}
	for _, k := range keys {
		names[k.ID] = k.Name
		info.Keys = append(info.Keys, KeyInfo{ID: k.ID, Name: k.Name, Key: k.String(), Default: k.Default,
			Env: envKey(k.ID), Signed: nonNilStrings(s.store().SignedWith(k.ID))})
	}
	s.updates.mu.Lock()
	if !s.updates.githubChecked.IsZero() {
		info.GitHubChecked = s.updates.githubChecked.Unix()
	}
	info.GitHubError = s.updates.githubError
	if time.Now().Before(s.updates.githubRetry) {
		info.GitHubRetry = s.updates.githubRetry.Unix()
	}
	s.updates.mu.Unlock()

	running := map[string][]string{} // version -> sites running it
	sites, _ := s.Network()
	for _, n := range sites {
		if n.Version != "" {
			running[n.Version] = append(running[n.Version], n.Name)
		}
	}
	// Notes of versions released before the release file carried them.
	var ghNotes map[string]string
	json.Unmarshal([]byte(s.Setting("github_notes")), &ghNotes)
	held := map[string]bool{}
	for _, in := range s.store().List(keys) {
		held[in.Version] = true
		v := VersionInfo{Version: in.Version, KeyID: in.KeyID, KeyName: names[in.KeyID], Released: in.Released,
			Current: in.Version == version.Version, Bad: in.Bad, Newer: release.Compare(in.Version, version.Version) > 0, Beta: release.Prerelease(in.Version),
			Sites: running[in.Version], Notes: in.Notes}
		if v.Notes == "" {
			v.Notes = ghNotes[in.Version]
		}
		if !v.Current {
			v.Blocker = s.compatBlocker(in.Manifest)
		}
		info.Versions = append(info.Versions, v)
		if v.Newer && !v.Bad && s.wanted(v.Version) && (info.Newer == "" || release.Compare(v.Version, info.Newer) > 0) {
			info.Newer = v.Version
		}
	}
	trusted := map[string]bool{}
	for _, k := range keys {
		trusted[k.ID] = true
	}
	for v, names := range running {
		if held[v] || release.Compare(v, version.Version) <= 0 || !s.wanted(v) {
			continue
		}
		slices.Sort(names)
		sv := SeenVersion{Version: v, Sites: names}
		for _, r := range s.peerReleases() {
			if r.ref.Version == v && trusted[r.ref.KeyID] {
				sv.Trusted = true
			}
		}
		s.updates.mu.Lock()
		sv.Error = s.updates.fetchErrors[v]
		s.updates.mu.Unlock()
		info.Seen = append(info.Seen, sv)
		if info.Newer == "" || release.Compare(v, info.Newer) > 0 {
			info.Newer = v
		}
	}
	slices.SortFunc(info.Seen, func(a, b SeenVersion) int { return release.Compare(b.Version, a.Version) })
	info.Old = s.oldVersions()
	return info
}

// OldVersions is what "Remove old versions" does: the versions older than
// the running one that go, the newest one this site can roll back to, which
// stays, and while the running version is on probation, the one the
// launcher goes back to if it fails, which stays too.
type OldVersions struct {
	Remove   []string `json:"remove"`
	Keep     string   `json:"keep,omitempty"`
	Fallback string   `json:"fallback,omitempty"`
}

func (s *Site) oldVersions() OldVersions {
	st := s.store()
	p := OldVersions{Remove: []string{}}
	if g := st.Good(); UnderLauncher() && g != "" && g != version.Version {
		p.Fallback = g
	}
	var older []release.Installed
	for _, in := range st.List(st.Keys()) {
		if release.Compare(in.Version, version.Version) < 0 {
			older = append(older, in)
		}
	}
	slices.SortFunc(older, func(a, b release.Installed) int { return release.Compare(b.Version, a.Version) })
	for _, in := range older {
		if !in.Bad && s.compatBlocker(in.Manifest) == "" {
			p.Keep = in.Version
			break
		}
	}
	for _, in := range older {
		if in.Version != p.Keep && in.Version != p.Fallback {
			p.Remove = append(p.Remove, in.Version)
		}
	}
	return p
}

// RemoveOldVersions removes the versions oldVersions lists.
func (s *Site) RemoveOldVersions() (OldVersions, error) {
	p := s.oldVersions()
	for _, v := range p.Remove {
		if err := s.store().Remove(v); err != nil {
			return p, fmt.Errorf("remove %s: %w", v, err)
		}
	}
	if len(p.Remove) > 0 {
		s.log.Info("removed old versions", "versions", strings.Join(p.Remove, ", "), "kept", p.Keep)
		s.notify(TopicPeers)
	}
	return p, nil
}

// SetGitHubChecks turns looking for new versions on GitHub on or off.
func (s *Site) SetGitHubChecks(on bool) {
	v := "0"
	if on {
		v = "1"
	}
	db.SetSetting(s.DB, "updates_github", v)
	if on {
		go s.CheckGitHubNow(context.Background())
	}
}

// Install runs an installed version: a newer one (update) or an older one
// (rollback), if it works with this site's database and every site in the
// IBS. IBS then exits and the launcher starts it; storage nodes follow.
func (s *Site) Install(v string) error {
	if !UnderLauncher() {
		return errors.New("this IBS wasn't started by the IBS launcher, so it can't switch versions; update the container image instead")
	}
	if v == version.Version {
		return errors.New("this version is already running")
	}
	_, in, err := s.store().Load(v, s.store().Keys())
	if err != nil {
		return err
	}
	if b := s.compatBlocker(in.Manifest); b != "" {
		return errors.New("can't run " + v + ": " + b)
	}
	if err := s.store().SetCurrent(v); err != nil {
		return err
	}
	s.log.Info("switching IBS version", "from", version.Version, "to", v)
	restartOnce.Do(func() { close(RestartRequested) })
	return nil
}

// RemoveVersion deletes an installed version that isn't running.
func (s *Site) RemoveVersion(v string) error {
	if v == version.Version {
		return errors.New("this version is running")
	}
	err := s.store().Remove(v)
	s.notify(TopicPeers)
	return err
}

// UploadRelease adds an .ibsrelease file signed by a trusted key.
func (s *Site) UploadRelease(r io.Reader) (release.Installed, error) {
	b, err := release.ReadTar(r)
	if err != nil {
		return release.Installed{}, err
	}
	in, err := s.store().Install(b, s.store().Keys())
	if err == nil && in.Version != version.Version {
		if why := s.dbBlocker(in.Manifest); why != "" {
			s.store().Remove(in.Version)
			return release.Installed{}, fmt.Errorf("IBS %s can't run on this site: %s", in.Version, why)
		}
	}
	if err == nil {
		s.log.Info("release uploaded", "version", in.Version, "key", in.KeyID)
		s.notify(TopicPeers)
	}
	return in, err
}

func (s *Site) AddReleaseKey(key, name string) (release.Key, error) {
	return s.store().AddKey(key, name)
}
func (s *Site) RemoveReleaseKey(id string) error {
	if envKey(id) {
		return errors.New("this key is set with IBS_TRUSTED_KEYS; remove it there and restart")
	}
	return s.store().RemoveKey(id)
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ReleaseBundle returns an installed version as an .ibsrelease file, for
// another site or one of this site's storage nodes.
func (s *Site) ReleaseBundle(v string) ([]byte, error) {
	b, _, err := s.store().Load(v, s.store().Keys())
	if err != nil {
		return nil, err
	}
	return b.BundleBytes(), nil
}

type peerRelease struct {
	site string
	ref  ReleaseRef
}

// peerReleases lists the versions connected sites hold.
func (s *Site) peerReleases() []peerRelease {
	rows, err := s.DB.Query(`SELECT site_id, releases FROM peers WHERE online = 1`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []peerRelease
	for rows.Next() {
		var id, js string
		rows.Scan(&id, &js)
		var refs []ReleaseRef
		json.Unmarshal([]byte(js), &refs)
		for _, r := range refs {
			out = append(out, peerRelease{id, r})
		}
	}
	return out
}

// noticeReleases is called when a connected site reports other versions than
// before. A new version this site would take is logged and fetched right away.
func (s *Site) noticeReleases(ctx context.Context, st SiteStatus) {
	newest := s.newestHeld()
	trusted := map[string]bool{}
	for _, k := range s.store().Keys() {
		trusted[k.ID] = true
	}
	found := false
	for _, r := range st.Releases {
		if r.Platform != release.Platform || !s.wanted(r.Version) || release.Compare(r.Version, newest) <= 0 {
			continue
		}
		found = found || trusted[r.KeyID]
		s.updates.mu.Lock()
		if s.updates.announced == nil {
			s.updates.announced = map[string]bool{}
		}
		first := !s.updates.announced[r.Version]
		s.updates.announced[r.Version] = true
		s.updates.mu.Unlock()
		if !first {
			continue
		}
		if trusted[r.KeyID] {
			s.log.Info("another site has a new version; fetching it", "version", r.Version, "from", st.Name)
		} else {
			s.log.Info("another site has a new version, signed with a key this site doesn't trust", "version", r.Version, "from", st.Name, "key", r.KeyID)
		}
	}
	if found {
		go s.fetchReleases(context.WithoutCancel(ctx))
	}
}

// newestHeld is the newest version this site holds that it would run, or the
// running one.
func (s *Site) newestHeld() string {
	newest := version.Version
	for _, in := range s.store().List(s.store().Keys()) {
		if !in.Bad && s.wanted(in.Version) && release.Compare(in.Version, newest) > 0 {
			newest = in.Version
		}
	}
	return newest
}

// fetchReleases gets newer versions this site doesn't hold yet: from
// connected sites (only when signed with a key this site trusts), and from
// GitHub when enabled. Fetching only stores them; nothing is installed.
func (s *Site) fetchReleases(ctx context.Context) {
	s.updates.fetchRun.Lock()
	defer s.updates.fetchRun.Unlock()
	s.removeUnrunnable()
	keys := s.store().Keys()
	trusted := map[string]bool{}
	for _, k := range keys {
		trusted[k.ID] = true
	}
	newest := s.newestHeld()
	for _, pr := range s.peerReleases() {
		r := pr.ref
		if !trusted[r.KeyID] || r.Platform != release.Platform || !s.wanted(r.Version) || release.Compare(r.Version, newest) <= 0 {
			continue
		}
		if !s.startFetch(r.Version) {
			continue
		}
		// A release is tens of megabytes: it may take as long as data keeps coming.
		var data []byte
		_, err := s.peerCall(ctx, pr.site, http.MethodGet, "/peer/v1/releases/"+r.Version, nil, &data)
		if err == nil {
			_, err = s.UploadRelease(bytes.NewReader(data))
		}
		s.endFetch(r.Version, err)
		if err != nil {
			s.log.Warn("fetch release from site", "version", r.Version, "site", pr.site[:8], "err", err)
			continue
		}
		s.log.Info("fetched new version from another site", "version", r.Version, "site", pr.site[:8])
		newest = r.Version
	}
	if s.GitHubChecks() && s.githubDue() {
		s.checkGitHub(ctx)
	}
	s.autoInstall()
}

// AutoInstall reports whether new versions are installed as soon as they are
// downloaded (off unless turned on under Settings → Updates).
func (s *Site) AutoInstall() bool { return s.Setting("updates_auto") == "1" }

// SetAutoInstall turns installing new versions automatically on or off.
func (s *Site) SetAutoInstall(on bool) {
	v := "0"
	if on {
		v = "1"
	}
	s.SetSetting("updates_auto", v)
	if on {
		go s.autoInstall()
	}
}

// autoInstall installs the newest downloaded version, when that is turned on:
// one newer than the running one, that counts as a new version (betas only
// when Include betas is on), hasn't failed to start here, and works with this
// site and the IBS. The site then restarts into it; its nodes follow.
func (s *Site) autoInstall() {
	if !s.AutoInstall() || !UnderLauncher() {
		return
	}
	best := ""
	for _, in := range s.store().List(s.store().Keys()) {
		if in.Bad || !s.wanted(in.Version) || release.Compare(in.Version, version.Version) <= 0 ||
			s.compatBlocker(in.Manifest) != "" {
			continue
		}
		if best == "" || release.Compare(in.Version, best) > 0 {
			best = in.Version
		}
	}
	if best == "" {
		return
	}
	s.log.Info("installing a new version automatically", "version", best)
	if err := s.Install(best); err != nil {
		s.log.Warn("install new version automatically", "version", best, "err", err)
	}
}

func (s *Site) startFetch(v string) bool {
	s.updates.mu.Lock()
	defer s.updates.mu.Unlock()
	if s.updates.fetching == nil {
		s.updates.fetching = map[string]bool{}
	}
	if s.updates.fetching[v] {
		return false
	}
	s.updates.fetching[v] = true
	return true
}

func (s *Site) endFetch(v string, err error) {
	s.updates.mu.Lock()
	delete(s.updates.fetching, v)
	if s.updates.fetchErrors == nil {
		s.updates.fetchErrors = map[string]string{}
	}
	if err != nil {
		s.updates.fetchErrors[v] = err.Error()
	} else {
		delete(s.updates.fetchErrors, v)
	}
	s.updates.mu.Unlock()
	s.notify(TopicPeers) // the Updates page shows the new version, or why it's missing
}

// releasesURL is where the latest release is announced (overridable for tests).
// CheckGitHubNow checks GitHub right away (the Updates page's "Check now"),
// unless GitHub's limit is used up.
func (s *Site) CheckGitHubNow(ctx context.Context) {
	s.updates.mu.Lock()
	limited := time.Now().Before(s.updates.githubRetry)
	s.updates.mu.Unlock()
	if s.GitHubChecks() && !limited {
		s.checkGitHub(ctx)
	}
}

// githubDue reports whether it's time to look at GitHub again: the check
// interval has passed since the last look, which is remembered across
// restarts (each restart would use up some of GitHub's limit), and GitHub's
// limit isn't used up.
func (s *Site) githubDue() bool {
	s.updates.mu.Lock()
	defer s.updates.mu.Unlock()
	if s.updates.githubChecked.IsZero() {
		if at := s.SettingInt("github_checked"); at > 0 {
			s.updates.githubChecked = time.Unix(at, 0)
		}
	}
	return time.Since(s.updates.githubChecked) > s.CheckInterval() && time.Now().After(s.updates.githubRetry)
}

// checkGitHub looks at the latest release on GitHub and downloads it when it
// is newer than what this site holds.
func (s *Site) checkGitHub(ctx context.Context) {
	// No total time limit: requests fail when they make no progress
	// (httpGet), so a slow line still gets there and a dead one fails fast.
	err := func() error {
		resp, err := httpGet(ctx, release.ReleasesURL())
		if err != nil {
			return err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if notes := release.GitHubNotes(body); len(notes) > 0 {
			if b, err := json.Marshal(notes); err == nil {
				db.SetSetting(s.DB, "github_notes", string(b))
			}
		}
		rel, err := release.PickGitHubRelease(body, s.Betas())
		if errors.Is(err, release.ErrNoRelease) {
			return nil // nothing to take (yet)
		}
		if err != nil {
			return err
		}
		v := rel.Version
		s.updates.mu.Lock()
		s.updates.githubLatest = v
		s.updates.mu.Unlock()
		for _, in := range s.store().List(s.store().Keys()) {
			if in.Version == v || (!in.Bad && release.Compare(in.Version, v) > 0) {
				return nil // already held, or something newer that works
			}
		}
		if release.Compare(version.Version, v) >= 0 {
			return nil
		}
		resp, err = httpGet(ctx, rel.AssetURL)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		in, err := s.UploadRelease(resp.Body)
		if err != nil {
			return err
		}
		s.log.Info("downloaded release from GitHub", "version", in.Version)
		return nil
	}()
	s.updates.mu.Lock()
	s.updates.githubChecked = time.Now()
	s.updates.githubError = ""
	if err != nil {
		s.updates.githubError = err.Error()
		var rl *rateLimited
		if errors.As(err, &rl) {
			s.updates.githubRetry = rl.until
			s.log.Warn("can't check GitHub for new versions until its limit resets", "until", rl.until.Format(time.RFC3339), "err", err)
		} else {
			s.log.Warn("check GitHub for releases", "err", err)
		}
	}
	s.updates.mu.Unlock()
	db.SetSetting(s.DB, "github_checked", strconv.FormatInt(time.Now().Unix(), 10))
	s.notify(TopicPeers)
}

func httpGet(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := stall.Do(http.DefaultClient, req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			if resp.Header.Get("X-RateLimit-Remaining") == "0" {
				reset, _ := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
				until := time.Unix(reset, 0)
				if reset == 0 {
					until = time.Now().Add(time.Hour)
				}
				return nil, &rateLimited{until}
			}
		}
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return resp, nil
}

// rateLimited: GitHub allows 60 requests an hour from one network address
// without signing in, and they're used up until then.
type rateLimited struct{ until time.Time }

func (e *rateLimited) Error() string {
	return "GitHub allows 60 checks an hour from your network (all computers on it together), and they're used up"
}

// nodeTarget is the version a storage node should run: the manager's, given
// to one outdated node at a time so the site never loses several at once.
func (s *Site) nodeTarget(nodeID, nodeVersion string) string {
	target := version.Version
	if !release.IsVersion(target) || nodeVersion == target {
		s.updates.mu.Lock()
		if s.updates.nodeUpdate.id == nodeID {
			s.updates.nodeUpdate.id = "" // done
		}
		s.updates.mu.Unlock()
		return ""
	}
	if _, err := os.Stat(s.store().Program(target)); err != nil {
		return "" // the manager doesn't hold its own release (e.g. started without the launcher)
	}
	s.updates.mu.Lock()
	defer s.updates.mu.Unlock()
	u := &s.updates.nodeUpdate
	if u.id != "" && u.id != nodeID && time.Since(u.since) < 10*time.Minute {
		return "" // another node is updating
	}
	if u.id != nodeID {
		u.id, u.since = nodeID, time.Now()
		s.log.Info("storage node updating", "node", nodeID, "from", nodeVersion, "to", target)
	}
	return target
}

// isNodeKey reports whether pub belongs to one of this site's storage nodes.
func (s *Site) isNodeKey(pub ed25519.PublicKey) bool {
	var n int
	s.DB.QueryRow(`SELECT COUNT(*) FROM nodes WHERE pubkey = ? AND is_local = 0`, []byte(pub)).Scan(&n)
	return n > 0
}

// trustedKeyStrings are the keys this site trusts, for its storage nodes.
func (s *Site) trustedKeyStrings() []string {
	var out []string
	for _, k := range s.store().Keys() {
		if !k.Default {
			out = append(out, k.String())
		}
	}
	return out
}

// noteDBLevel records the database level this version uses, so an older
// version that can't read it is refused as a rollback.
func (s *Site) noteDBLevel() {
	if int64(db.Level) > s.SettingInt("db_level") {
		db.SetSetting(s.DB, "db_level", fmt.Sprint(db.Level))
	}
}
