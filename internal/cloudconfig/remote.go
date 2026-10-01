package cloudconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
)

// globalStateKey is the sync_state key of the global remote. It must stay equal
// to store.DefaultSyncTargetKey; it is duplicated here so cloudconfig remains a
// leaf package without a store dependency.
const globalStateKey = "cloud"

// RemoteStateKeyPrefix prefixes the sync_state key of every per-project remote.
// The full key is RemoteStateKeyPrefix followed by the remote's RemoteID.
const RemoteStateKeyPrefix = "cloud@"

// NamedProjectRemote pairs a per-project override with its project name.
type NamedProjectRemote struct {
	Project string
	ProjectRemote
}

// Remote is the resolved cloud destination for a project.
type Remote struct {
	ServerURL string
	Token     string
	// ID is the RemoteID of a per-project override; empty for the global remote.
	ID string
	// Global reports whether the project uses the global remote.
	Global bool
	// ServerSource and TokenSource report where each value came from. Env
	// sources only occur for the global remote.
	ServerSource Source
	TokenSource  Source
}

// SetProjectRemote routes project to its own remote. The server URL is
// validated and stored in its validated form; the token may be empty.
func SetProjectRemote(cfg *Config, project, serverURL, token string) error {
	if cfg == nil {
		return fmt.Errorf("cloud config is required")
	}
	name := strings.TrimSpace(project)
	if name == "" {
		return fmt.Errorf("project is required")
	}
	validated, err := ValidateServerURL(serverURL)
	if err != nil {
		return fmt.Errorf("invalid server URL: %w", err)
	}
	if cfg.Projects == nil {
		cfg.Projects = make(map[string]ProjectRemote)
	}
	cfg.Projects[name] = ProjectRemote{ServerURL: validated, Token: strings.TrimSpace(token)}
	return nil
}

// ClearProjectRemote removes the override for project and reports whether one
// existed. The map is dropped when it becomes empty so cloud.json keeps the
// legacy shape.
func ClearProjectRemote(cfg *Config, project string) bool {
	if cfg == nil || cfg.Projects == nil {
		return false
	}
	name := strings.TrimSpace(project)
	if _, ok := cfg.Projects[name]; !ok {
		return false
	}
	delete(cfg.Projects, name)
	if len(cfg.Projects) == 0 {
		cfg.Projects = nil
	}
	return true
}

// ProjectRemotes lists the per-project overrides sorted by project name.
func ProjectRemotes(cfg *Config) []NamedProjectRemote {
	if cfg == nil || len(cfg.Projects) == 0 {
		return nil
	}
	out := make([]NamedProjectRemote, 0, len(cfg.Projects))
	for name, remote := range cfg.Projects {
		out = append(out, NamedProjectRemote{Project: name, ProjectRemote: remote})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Project < out[j].Project })
	return out
}

// ResolveForProject returns the remote project syncs against. Projects without
// an override use the global remote with the usual env overrides
// (ENGRAM_CLOUD_SERVER, then ENGRAM_CLOUD_TOKEN before cloud.json). Overridden
// projects ignore env vars and never fall back to the global token, so the
// global credential is not sent to a different server.
func ResolveForProject(cfg *Config, project string) Remote {
	if cfg != nil {
		if override, ok := cfg.Projects[strings.TrimSpace(project)]; ok && strings.TrimSpace(project) != "" {
			return resolveOverride(override)
		}
	}
	return resolveGlobal(cfg)
}

func resolveOverride(override ProjectRemote) Remote {
	r := Remote{
		ServerURL: strings.TrimSpace(override.ServerURL),
		Token:     strings.TrimSpace(override.Token),
	}
	if r.ServerURL != "" {
		r.ServerSource = SourceFile
	}
	if r.Token != "" {
		r.TokenSource = SourceFile
	}
	r.ID = RemoteID(r.ServerURL, r.Token)
	return r
}

func resolveGlobal(cfg *Config) Remote {
	r := Remote{Global: true}
	if cfg != nil {
		r.ServerURL = cfg.ServerURL
		r.Token = strings.TrimSpace(cfg.Token)
	}
	if strings.TrimSpace(r.ServerURL) != "" {
		r.ServerSource = SourceFile
	}
	if value := strings.TrimSpace(os.Getenv(EnvCloudServer)); value != "" {
		r.ServerURL = value
		r.ServerSource = SourceEnv
	}
	if r.Token != "" {
		r.TokenSource = SourceFile
	}
	if value := strings.TrimSpace(os.Getenv(EnvCloudToken)); value != "" {
		r.Token = value
		r.TokenSource = SourceEnv
	}
	return r
}

// RemoteID returns a stable 12-hex-character identifier for a server URL and
// token pair. The URL is trimmed, its scheme and host lowercased, and trailing
// slashes removed before hashing, so the id is safe inside a sync_state key.
func RemoteID(serverURL, token string) string {
	sum := sha256.Sum256([]byte(normalizeServerURL(serverURL) + "\x00" + strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])[:12]
}

func normalizeServerURL(raw string) string {
	value := strings.TrimSpace(raw)
	if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
		parsed.Scheme = strings.ToLower(parsed.Scheme)
		parsed.Host = strings.ToLower(parsed.Host)
		value = parsed.String()
	}
	return strings.TrimRight(value, "/")
}

// StateKey returns the sync_state key for r: "cloud" for the global remote and
// RemoteStateKeyPrefix plus the remote id otherwise.
func StateKey(r Remote) string {
	if r.Global {
		return globalStateKey
	}
	return RemoteStateKeyPrefix + r.ID
}
