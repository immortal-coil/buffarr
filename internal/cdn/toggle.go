// Package cdn implements the CDN platform enable/disable toggle (R8, R9):
// the single enabled-CDN set (KTD3) is persisted in the catalog and drives
// both dnsmasq's two config files (U3) and nginx's Host-header allow-list
// (U2) from one toggle action, so DNS-layer and nginx-layer enforcement
// never disagree.
package cdn

import (
	"fmt"
	"log"
	"os"
	"sort"

	"github.com/immortal-coil/buffarr/internal/catalog"
	"github.com/immortal-coil/buffarr/internal/dnsmasq"
	"github.com/immortal-coil/buffarr/internal/nginx"
)

// Toggler owns the enabled-CDN set's source of truth (the catalog's
// cdn_platforms table) and the three files that must always reflect it.
type Toggler struct {
	cat           *catalog.Catalog
	platforms     []dnsmasq.Platform
	cacheServerIP string

	dnsmasqAddnHostsPath    string
	dnsmasqWildcardConfPath string
	nginxAllowlistPath      string
}

func NewToggler(cat *catalog.Catalog, platforms []dnsmasq.Platform, cacheServerIP, dnsmasqAddnHostsPath, dnsmasqWildcardConfPath, nginxAllowlistPath string) *Toggler {
	return &Toggler{
		cat:                     cat,
		platforms:               platforms,
		cacheServerIP:           cacheServerIP,
		dnsmasqAddnHostsPath:    dnsmasqAddnHostsPath,
		dnsmasqWildcardConfPath: dnsmasqWildcardConfPath,
		nginxAllowlistPath:      nginxAllowlistPath,
	}
}

// PlatformStatus is one row of R8's "view all CDN platforms, see which are
// enabled."
type PlatformStatus struct {
	Name        string
	Description string
	Enabled     bool
}

// ListPlatforms returns every known platform with its current enabled
// status (R8), ordered by name.
func (t *Toggler) ListPlatforms() ([]PlatformStatus, error) {
	enabledNames, err := t.cat.EnabledPlatformNames()
	if err != nil {
		return nil, fmt.Errorf("listing enabled platforms: %w", err)
	}
	enabled := make(map[string]struct{}, len(enabledNames))
	for _, n := range enabledNames {
		enabled[n] = struct{}{}
	}

	statuses := make([]PlatformStatus, len(t.platforms))
	for i, p := range t.platforms {
		_, isEnabled := enabled[p.Name]
		statuses[i] = PlatformStatus{Name: p.Name, Description: p.Description, Enabled: isEnabled}
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
	return statuses, nil
}

// Toggle enables or disables one platform (R9), regenerating and writing
// dnsmasq's and nginx's config from the resulting set, and only then
// persisting the change. If name isn't a known platform, this is an error
// -- R9 scopes toggling to "a supported CDN platform."
//
// If any file write fails, every layer is reverted to its prior content
// and the catalog is left untouched (KTD3's "revert both layers" -- in
// practice three files, since U3's hybrid SIGHUP/restart design split
// dnsmasq into two). A retry is therefore always safe to re-attempt: the
// next call starts from the same prior state, not a half-applied one.
func (t *Toggler) Toggle(name string, enabled bool) error {
	if !t.isKnownPlatform(name) {
		return fmt.Errorf("unknown CDN platform %q", name)
	}

	current, err := t.cat.EnabledPlatformNames()
	if err != nil {
		return fmt.Errorf("reading current enabled set: %w", err)
	}

	if err := t.applyEnabledSet(applyToggle(current, name, enabled)); err != nil {
		return err
	}

	// Persisted last (see applyEnabledSet's doc comment): by this point
	// every file write has already succeeded, so the database is never
	// left asserting a state the running config doesn't reflect.
	return t.cat.SetPlatformEnabled(name, enabled)
}

// RegenerateFromPersisted re-renders and re-writes all three files from
// the currently persisted enabled set, without changing that set. Called
// once at backend startup (the startup-assert step persistence requires
// to be meaningful): without it, a restarted backend would leave
// nginx/dnsmasq running whatever config a *previous* backend instance (or
// their own bootstrap-permissive defaults, KTD7) last wrote, silently
// ignoring the operator's persisted toggles until the next manual toggle.
func (t *Toggler) RegenerateFromPersisted() error {
	enabled, err := t.cat.EnabledPlatformNames()
	if err != nil {
		return fmt.Errorf("reading persisted enabled set: %w", err)
	}
	return t.applyEnabledSet(enabled)
}

// applyEnabledSet renders dnsmasq's and nginx's config for enabledNames
// and writes all three files, reverting every already-written file to its
// prior content if any write fails. Rendering happens entirely in memory
// before any I/O, so a render error (e.g. a malformed cacheServerIP) aborts
// before touching anything on disk.
func (t *Toggler) applyEnabledSet(enabledNames []string) error {
	addnHosts, wildcardConf, err := dnsmasq.Generate(t.cacheServerIP, t.platforms, enabledNames)
	if err != nil {
		return fmt.Errorf("rendering dnsmasq config: %w", err)
	}

	exact, wildcard := dnsmasq.EnabledDomains(t.platforms, enabledNames)
	entries := make([]nginx.AllowlistEntry, 0, len(exact)+len(wildcard))
	for _, d := range exact {
		entries = append(entries, nginx.AllowlistEntry{Domain: d})
	}
	for _, d := range wildcard {
		entries = append(entries, nginx.AllowlistEntry{Domain: d, Wildcard: true})
	}
	allowlist, skipped := nginx.GenerateAllowlist(entries)
	if len(skipped) > 0 {
		// The vendored domain data is already validated at load time
		// (internal/dnsmasq.LoadPlatforms), so a skip here would mean that
		// validation and this one have drifted -- worth knowing about, but
		// not worth blocking the toggle over.
		log.Printf("cdn: %d domains skipped by nginx allow-list validation: %v", len(skipped), skipped)
	}

	type fileWrite struct {
		path    string
		content string
		write   func(path, content string) error
	}
	writes := []fileWrite{
		{t.dnsmasqAddnHostsPath, addnHosts, dnsmasq.WriteFile},
		{t.dnsmasqWildcardConfPath, wildcardConf, dnsmasq.WriteFile},
		{t.nginxAllowlistPath, allowlist, nginx.WriteAllowlist},
	}

	prior := make(map[string]string, len(writes))
	for _, w := range writes {
		content, err := os.ReadFile(w.path)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("reading current content of %s: %w", w.path, err)
		}
		prior[w.path] = string(content)
	}

	var written []fileWrite
	for _, w := range writes {
		if err := w.write(w.path, w.content); err != nil {
			for _, done := range written {
				if revertErr := done.write(done.path, prior[done.path]); revertErr != nil {
					log.Printf("cdn: failed to revert %s after a partial toggle failure: %v", done.path, revertErr)
				}
			}
			return fmt.Errorf("writing %s: %w (reverted %d prior write(s))", w.path, err, len(written))
		}
		written = append(written, w)
	}

	return nil
}

func (t *Toggler) isKnownPlatform(name string) bool {
	for _, p := range t.platforms {
		if p.Name == name {
			return true
		}
	}
	return false
}

func applyToggle(current []string, name string, enabled bool) []string {
	set := make(map[string]struct{}, len(current)+1)
	for _, n := range current {
		set[n] = struct{}{}
	}
	if enabled {
		set[name] = struct{}{}
	} else {
		delete(set, name)
	}

	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	return out
}
