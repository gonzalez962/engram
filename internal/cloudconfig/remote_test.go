package cloudconfig

import (
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestLoadLegacyFileWithoutProjects(t *testing.T) {
	dataDir := t.TempDir()
	raw := []byte(`{"server_url":"https://cloud.example.test","token":"file-token"}`)
	if err := os.WriteFile(Path(dataDir), raw, 0o600); err != nil {
		t.Fatalf("seed legacy cloud.json: %v", err)
	}

	got, err := Load(dataDir)
	if err != nil {
		t.Fatalf("load legacy cloud.json: %v", err)
	}
	if got.ServerURL != "https://cloud.example.test" || got.Token != "file-token" {
		t.Fatalf("legacy config = %+v", got)
	}
	if got.Projects != nil {
		t.Fatalf("legacy config projects = %#v, want nil", got.Projects)
	}
}

func TestSaveWithoutProjectsKeepsLegacyShape(t *testing.T) {
	dataDir := t.TempDir()
	if err := Save(dataDir, &Config{ServerURL: "https://cloud.example.test", Token: "t"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	data, err := os.ReadFile(Path(dataDir))
	if err != nil {
		t.Fatalf("read cloud.json: %v", err)
	}
	if strings.Contains(string(data), "projects") {
		t.Fatalf("cloud.json without overrides contains projects key:\n%s", data)
	}
	want := "{\n  \"server_url\": \"https://cloud.example.test\",\n  \"token\": \"t\"\n}"
	if string(data) != want {
		t.Fatalf("cloud.json = %q, want %q", data, want)
	}
}

func TestSaveLoadRoundTripWithProjects(t *testing.T) {
	dataDir := t.TempDir()
	cfg := &Config{ServerURL: "https://global.example.test", Token: "global-token"}
	if err := SetProjectRemote(cfg, "alpha", "https://alpha.example.test", "alpha-token"); err != nil {
		t.Fatalf("set alpha: %v", err)
	}
	if err := SetProjectRemote(cfg, "beta", "https://beta.example.test", ""); err != nil {
		t.Fatalf("set beta: %v", err)
	}
	if err := Save(dataDir, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	info, err := os.Stat(Path(dataDir))
	if err != nil {
		t.Fatalf("stat cloud.json: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 && !isWindows() {
		t.Fatalf("cloud.json mode = %v, want owner-only", info.Mode().Perm())
	}

	got, err := Load(dataDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reflect.DeepEqual(got, cfg) {
		t.Fatalf("round trip = %+v, want %+v", got, cfg)
	}
	data, _ := os.ReadFile(Path(dataDir))
	if strings.Count(string(data), `"token"`) != 2 {
		t.Fatalf("empty override token should be omitted:\n%s", data)
	}
}

func TestSetProjectRemote(t *testing.T) {
	tests := []struct {
		name      string
		project   string
		serverURL string
		token     string
		wantErr   bool
		wantKey   string
		wantURL   string
		wantToken string
	}{
		{name: "valid trims inputs", project: "  alpha ", serverURL: " https://alpha.example.test/ ", token: "  tok  ", wantKey: "alpha", wantURL: "https://alpha.example.test/", wantToken: "tok"},
		{name: "empty token allowed", project: "beta", serverURL: "http://localhost:8080", wantKey: "beta", wantURL: "http://localhost:8080"},
		{name: "empty project", project: "   ", serverURL: "https://alpha.example.test", wantErr: true},
		{name: "empty url", project: "alpha", serverURL: "", wantErr: true},
		{name: "bad scheme", project: "alpha", serverURL: "ftp://alpha.example.test", wantErr: true},
		{name: "query rejected", project: "alpha", serverURL: "https://alpha.example.test?x=1", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{}
			err := SetProjectRemote(cfg, tt.project, tt.serverURL, tt.token)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("SetProjectRemote error = nil, want error")
				}
				if cfg.Projects != nil {
					t.Fatalf("failed set mutated projects: %#v", cfg.Projects)
				}
				return
			}
			if err != nil {
				t.Fatalf("SetProjectRemote: %v", err)
			}
			got, ok := cfg.Projects[tt.wantKey]
			if !ok {
				t.Fatalf("projects = %#v, missing %q", cfg.Projects, tt.wantKey)
			}
			if got.ServerURL != tt.wantURL || got.Token != tt.wantToken {
				t.Fatalf("override = %+v, want url %q token %q", got, tt.wantURL, tt.wantToken)
			}
		})
	}

	if err := SetProjectRemote(nil, "alpha", "https://alpha.example.test", ""); err == nil {
		t.Fatalf("SetProjectRemote(nil) error = nil, want error")
	}
}

func TestClearProjectRemote(t *testing.T) {
	cfg := &Config{}
	if ClearProjectRemote(cfg, "alpha") {
		t.Fatalf("clear on empty config = true")
	}
	if ClearProjectRemote(nil, "alpha") {
		t.Fatalf("clear on nil config = true")
	}
	_ = SetProjectRemote(cfg, "alpha", "https://alpha.example.test", "a")
	_ = SetProjectRemote(cfg, "beta", "https://beta.example.test", "b")

	if !ClearProjectRemote(cfg, " alpha ") {
		t.Fatalf("clear existing alpha = false")
	}
	if ClearProjectRemote(cfg, "alpha") {
		t.Fatalf("second clear of alpha = true")
	}
	if len(cfg.Projects) != 1 {
		t.Fatalf("projects after clearing alpha = %#v", cfg.Projects)
	}
	if !ClearProjectRemote(cfg, "beta") {
		t.Fatalf("clear existing beta = false")
	}
	if cfg.Projects != nil {
		t.Fatalf("projects after clearing all = %#v, want nil", cfg.Projects)
	}
}

func TestProjectRemotesSorted(t *testing.T) {
	if got := ProjectRemotes(nil); len(got) != 0 {
		t.Fatalf("ProjectRemotes(nil) = %#v", got)
	}
	cfg := &Config{}
	_ = SetProjectRemote(cfg, "zeta", "https://z.example.test", "z")
	_ = SetProjectRemote(cfg, "alpha", "https://a.example.test", "a")
	_ = SetProjectRemote(cfg, "mid", "https://m.example.test", "")

	got := ProjectRemotes(cfg)
	names := make([]string, 0, len(got))
	for _, r := range got {
		names = append(names, r.Project)
	}
	if !reflect.DeepEqual(names, []string{"alpha", "mid", "zeta"}) {
		t.Fatalf("project order = %v", names)
	}
	if got[0].ServerURL != "https://a.example.test" || got[0].Token != "a" {
		t.Fatalf("first remote = %+v", got[0])
	}
}

func TestResolveForProjectGlobal(t *testing.T) {
	base := func() *Config {
		cfg := &Config{ServerURL: "https://global.example.test", Token: " global-token "}
		_ = SetProjectRemote(cfg, "routed", "https://routed.example.test", "routed-token")
		return cfg
	}
	tests := []struct {
		name       string
		cfg        *Config
		project    string
		envServer  string
		envToken   string
		wantURL    string
		wantToken  string
		wantServer Source
		wantTokSrc Source
	}{
		{name: "unlisted project uses file", cfg: base(), project: "other", wantURL: "https://global.example.test", wantToken: "global-token", wantServer: SourceFile, wantTokSrc: SourceFile},
		{name: "empty project uses global", cfg: base(), project: "", wantURL: "https://global.example.test", wantToken: "global-token", wantServer: SourceFile, wantTokSrc: SourceFile},
		{name: "env overrides global", cfg: base(), project: "other", envServer: "https://env.example.test", envToken: "env-token", wantURL: "https://env.example.test", wantToken: "env-token", wantServer: SourceEnv, wantTokSrc: SourceEnv},
		{name: "nil config nothing set", cfg: nil, project: "other", wantServer: SourceNone, wantTokSrc: SourceNone},
		{name: "nil config env only", cfg: nil, project: "x", envServer: "https://env.example.test", envToken: "env-token", wantURL: "https://env.example.test", wantToken: "env-token", wantServer: SourceEnv, wantTokSrc: SourceEnv},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvCloudServer, tt.envServer)
			t.Setenv(EnvCloudToken, tt.envToken)
			got := ResolveForProject(tt.cfg, tt.project)
			if !got.Global || got.ID != "" {
				t.Fatalf("remote = %+v, want global with empty id", got)
			}
			if got.ServerURL != tt.wantURL || got.Token != tt.wantToken {
				t.Fatalf("remote = %+v, want url %q token %q", got, tt.wantURL, tt.wantToken)
			}
			if got.ServerSource != tt.wantServer || got.TokenSource != tt.wantTokSrc {
				t.Fatalf("sources = %v/%v, want %v/%v", got.ServerSource, got.TokenSource, tt.wantServer, tt.wantTokSrc)
			}
			if StateKey(got) != "cloud" {
				t.Fatalf("StateKey(global) = %q, want cloud", StateKey(got))
			}
		})
	}
}

func TestResolveForProjectOverrideIgnoresEnvAndGlobalToken(t *testing.T) {
	t.Setenv(EnvCloudServer, "https://env.example.test")
	t.Setenv(EnvCloudToken, "env-token")
	cfg := &Config{ServerURL: "https://global.example.test", Token: "global-token"}
	if err := SetProjectRemote(cfg, "routed", "https://routed.example.test", "routed-token"); err != nil {
		t.Fatalf("set routed: %v", err)
	}
	if err := SetProjectRemote(cfg, "tokenless", "https://tokenless.example.test", ""); err != nil {
		t.Fatalf("set tokenless: %v", err)
	}

	routed := ResolveForProject(cfg, " routed ")
	if routed.Global {
		t.Fatalf("routed remote is global: %+v", routed)
	}
	if routed.ServerURL != "https://routed.example.test" || routed.Token != "routed-token" {
		t.Fatalf("routed remote = %+v", routed)
	}
	if routed.ServerSource != SourceFile || routed.TokenSource != SourceFile {
		t.Fatalf("routed sources = %v/%v, want file/file", routed.ServerSource, routed.TokenSource)
	}
	if routed.ID != RemoteID("https://routed.example.test", "routed-token") || routed.ID == "" {
		t.Fatalf("routed id = %q", routed.ID)
	}
	if StateKey(routed) != RemoteStateKeyPrefix+routed.ID {
		t.Fatalf("StateKey(routed) = %q", StateKey(routed))
	}

	tokenless := ResolveForProject(cfg, "tokenless")
	if tokenless.Global || tokenless.Token != "" || tokenless.TokenSource != SourceNone {
		t.Fatalf("tokenless remote leaked a credential: %+v", tokenless)
	}
	if tokenless.ServerURL != "https://tokenless.example.test" {
		t.Fatalf("tokenless server = %q", tokenless.ServerURL)
	}
}

func TestRemoteID(t *testing.T) {
	id := RemoteID("https://cloud.example.test", "tok")
	if len(id) != 12 {
		t.Fatalf("RemoteID length = %d (%q), want 12", len(id), id)
	}
	for _, r := range id {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("RemoteID %q contains non-hex rune %q", id, r)
		}
	}
	equal := []struct{ name, url, token string }{
		{"same inputs", "https://cloud.example.test", "tok"},
		{"trailing slash", "https://cloud.example.test/", "tok"},
		{"surrounding space", "  https://cloud.example.test  ", "tok"},
		{"uppercase scheme and host", "HTTPS://Cloud.Example.TEST", "tok"},
	}
	for _, tt := range equal {
		if got := RemoteID(tt.url, tt.token); got != id {
			t.Fatalf("%s: RemoteID = %q, want %q", tt.name, got, id)
		}
	}
	different := []struct{ name, url, token string }{
		{"different host", "https://other.example.test", "tok"},
		{"different path", "https://cloud.example.test/api", "tok"},
		{"different token", "https://cloud.example.test", "tok2"},
		{"empty token", "https://cloud.example.test", ""},
		{"different scheme", "http://cloud.example.test", "tok"},
	}
	for _, tt := range different {
		if got := RemoteID(tt.url, tt.token); got == id {
			t.Fatalf("%s: RemoteID collided with base id %q", tt.name, id)
		}
	}
	if RemoteID("https://a.example.test/b", "") == RemoteID("https://a.example.test", "/b") {
		t.Fatalf("RemoteID must separate URL and token")
	}
}

func TestStateKey(t *testing.T) {
	if got := StateKey(Remote{Global: true}); got != "cloud" {
		t.Fatalf("StateKey(global) = %q", got)
	}
	if got := StateKey(Remote{ID: "abc123def456"}); got != "cloud@abc123def456" {
		t.Fatalf("StateKey(override) = %q", got)
	}
	if RemoteStateKeyPrefix != "cloud@" {
		t.Fatalf("RemoteStateKeyPrefix = %q", RemoteStateKeyPrefix)
	}
}

func isWindows() bool { return runtime.GOOS == "windows" }
