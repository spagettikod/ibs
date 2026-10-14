package tlscert

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The HTTPS state (settings, certificate and ACME account) is kept in the
// site's metadata as well, so it is backed up to the site's nodes and peers
// with everything else, encrypted with the site's key, and comes back when
// the site is restored.

// Export returns the HTTPS state as one blob: the files of the manager's
// directory by relative path.
func (m *Manager) Export() ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	files := map[string]string{}
	err := filepath.WalkDir(m.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(p, ".tmp") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(m.dir, p)
		files[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(files)
}

// Import restores HTTPS state from a backup when this manager has none of its
// own (no settings and no certificate), e.g. after a restore, and loads it.
// It reports whether it did.
func (m *Manager) Import(blob []byte) (bool, error) {
	if len(blob) == 0 {
		return false, nil
	}
	if _, err := os.Stat(m.settingsPath()); err == nil {
		return false, nil
	}
	if _, err := os.Stat(m.path("cert.pem")); err == nil {
		return false, nil
	}
	var files map[string]string
	if err := json.Unmarshal(blob, &files); err != nil {
		return false, err
	}
	for rel, data := range files {
		p := filepath.Join(m.dir, filepath.FromSlash(rel))
		if !strings.HasPrefix(p, m.dir+string(filepath.Separator)) {
			return false, errors.New("invalid path in HTTPS backup")
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return false, err
		}
		if err := writeFile(p, []byte(data)); err != nil {
			return false, err
		}
	}
	m.reload()
	m.log.Info("HTTPS settings restored from the site's backup", "domains", m.Status().Domains)
	m.kick()
	return true, nil
}

// reload reads the stored settings and certificate again (not with settings
// from environment variables).
func (m *Manager) reload() {
	if m.fromEnv {
		m.mu.Lock()
		m.loadCert()
		m.mu.Unlock()
		return
	}
	var set Settings
	if b, err := os.ReadFile(m.settingsPath()); err == nil {
		json.Unmarshal(b, &set)
	}
	p, err := validate(set)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.set, m.provider, m.err = set, p, nil
	if err != nil {
		m.err, m.errTime = err, time.Now()
	}
	m.loadCert()
}

func (m *Manager) kick() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// OnChange sets a function to call after the settings or the certificate
// changed, to back up the new state (see Export).
func (m *Manager) OnChange(f func()) {
	m.mu.Lock()
	m.onChange = f
	m.mu.Unlock()
}

func (m *Manager) changed() {
	m.mu.RLock()
	f := m.onChange
	m.mu.RUnlock()
	if f != nil {
		f()
	}
}
