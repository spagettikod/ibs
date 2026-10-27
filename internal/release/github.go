package release

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

// AssetSuffix is how release files for this platform are named on GitHub:
// ibs-<version>-linux-amd64.ibsrelease.
var AssetSuffix = "-" + strings.ReplaceAll(Platform, "/", "-") + ".ibsrelease"

// ReleasesURL is where IBS releases are listed: GitHub's release list, or
// IBS_RELEASES_URL (a list, or a single release in the same format).
func ReleasesURL() string {
	if u := os.Getenv("IBS_RELEASES_URL"); u != "" {
		return u
	}
	return "https://api.github.com/repos/spagettikod/ibs/releases?per_page=30"
}

// Prerelease reports whether v is a beta or other pre-release (or not a
// version at all, like "dev").
func Prerelease(v string) bool { return !IsVersion(v) || strings.Contains(v, "-") }

// ErrNoRelease means GitHub lists no release that fits.
var ErrNoRelease = errors.New("no release found")

// GitHubNotes returns the release notes of each release in GitHub's answer,
// by version: for versions released before the notes were in the release file.
func GitHubNotes(body []byte) map[string]string {
	type ghRelease struct {
		TagName string `json:"tag_name"`
		Body    string `json:"body"`
	}
	var list []ghRelease
	if json.Unmarshal(body, &list) != nil {
		var one ghRelease
		if json.Unmarshal(body, &one) != nil {
			return nil
		}
		list = []ghRelease{one}
	}
	out := map[string]string{}
	for _, r := range list {
		if v := strings.TrimPrefix(r.TagName, "v"); IsVersion(v) && strings.TrimSpace(r.Body) != "" {
			out[v] = strings.TrimSpace(r.Body)
		}
	}
	return out
}

// GitHubRelease is a release found on GitHub.
type GitHubRelease struct {
	Version  string
	AssetURL string // the .ibsrelease file for this platform
}

// PickGitHubRelease reads GitHub's answer (a list of releases, or one
// release) and returns the newest release with a file for this platform.
// Betas and other pre-releases count only when betas is true.
func PickGitHubRelease(body []byte, betas bool) (GitHubRelease, error) {
	list, err := GitHubReleases(body, betas)
	if err != nil {
		return GitHubRelease{}, err
	}
	return list[0], nil
}

// GitHubReleases is like PickGitHubRelease, but returns every release that
// fits, newest first.
func GitHubReleases(body []byte, betas bool) ([]GitHubRelease, error) {
	type ghRelease struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	var list []ghRelease
	if err := json.Unmarshal(body, &list); err != nil {
		var one ghRelease
		if err := json.Unmarshal(body, &one); err != nil {
			return nil, fmt.Errorf("unexpected answer from GitHub: %w", err)
		}
		list = []ghRelease{one}
	}
	var out []GitHubRelease
	for _, r := range list {
		v := strings.TrimPrefix(r.TagName, "v")
		if r.Draft || !IsVersion(v) || (!betas && (r.Prerelease || Prerelease(v))) {
			continue
		}
		for _, a := range r.Assets {
			if strings.HasSuffix(a.Name, AssetSuffix) {
				out = append(out, GitHubRelease{Version: v, AssetURL: a.URL})
				break
			}
		}
	}
	if len(out) == 0 {
		kind := "release"
		if !betas {
			kind = "stable release"
		}
		return nil, fmt.Errorf("%w: no %s with a %s file", ErrNoRelease, kind, AssetSuffix)
	}
	slices.SortStableFunc(out, func(a, b GitHubRelease) int { return Compare(b.Version, a.Version) })
	return out, nil
}
