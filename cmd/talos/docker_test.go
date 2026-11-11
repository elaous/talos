package main

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/elaous/talos/config"
	"gopkg.in/yaml.v3"
)

type composeFile struct {
	Services map[string]struct {
		Image       string            `yaml:"image"`
		User        string            `yaml:"user"`
		Ports       []string          `yaml:"ports"`
		Volumes     []string          `yaml:"volumes"`
		Environment map[string]string `yaml:"environment"`
		ReadOnly    bool              `yaml:"read_only"`
		CapDrop     []string          `yaml:"cap_drop"`
		SecurityOpt []string          `yaml:"security_opt"`
		Profiles    []string          `yaml:"profiles"`
	} `yaml:"services"`
	Volumes map[string]any `yaml:"volumes"`
}

func readCompose(t *testing.T, path string) composeFile {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c composeFile
	if err := yaml.Unmarshal(b, &c); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return c
}

// Compose security settings are parsed values, not text matches.
func TestComposeSecurityPosture(t *testing.T) {
	c := readCompose(t, "../../docker-compose.yml")
	talos, ok := c.Services["talos"]
	if !ok {
		t.Fatal("no talos service")
	}
	if len(talos.Ports) != 1 || talos.Ports[0] != "127.0.0.1:8080:8080" {
		t.Errorf("talos has no login: its port must be published on loopback only, got %v", talos.Ports)
	}
	if !talos.ReadOnly {
		t.Error("the root filesystem is not read-only")
	}
	if len(talos.CapDrop) != 1 || talos.CapDrop[0] != "ALL" {
		t.Errorf("capabilities: %v", talos.CapDrop)
	}
	if !slices_contains(talos.SecurityOpt, "no-new-privileges:true") {
		t.Errorf("security_opt: %v", talos.SecurityOpt)
	}
	if talos.User != "" {
		t.Errorf("the default setup must run as the image's user, which owns the named volumes; got user %q", talos.User)
	}
	want := []string{"talos-pdfs:/pdfs", "talos-data:/data"}
	for _, w := range want {
		if !slices_contains(talos.Volumes, w) {
			t.Errorf("volume %s missing from %v", w, talos.Volumes)
		}
	}
	for _, v := range talos.Volumes {
		if strings.Contains(v, "docker.sock") || strings.HasPrefix(v, "/") || strings.HasPrefix(v, ".") {
			t.Errorf("unexpected host mount %q in the default setup", v)
		}
	}
	for _, name := range []string{"talos-pdfs", "talos-data"} {
		if _, ok := c.Volumes[name]; !ok {
			t.Errorf("named volume %s is not declared", name)
		}
	}
	// optional Ollama service: profile-gated, unpublished, pinned
	ol := c.Services["ollama"]
	if len(ol.Ports) != 0 || !slices_contains(ol.Profiles, "ollama") {
		t.Errorf("ollama: ports %v profiles %v", ol.Ports, ol.Profiles)
	}
	if !regexp.MustCompile(`^ollama/ollama:\d+\.\d+\.\d+$`).MatchString(ol.Image) {
		t.Errorf("the ollama image is not pinned to a release: %q", ol.Image)
	}
}

func TestComposeHostDirsOverride(t *testing.T) {
	c := readCompose(t, "../../docker-compose.hostdirs.yml")
	talos := c.Services["talos"]
	if !strings.Contains(talos.User, "TALOS_UID") || !strings.Contains(talos.User, "TALOS_GID") {
		t.Errorf("the override must run the container as the folders' owner: %q", talos.User)
	}
	for _, w := range []string{"./pdfs:/pdfs", "./data:/data"} {
		if !slices_contains(talos.Volumes, w) {
			t.Errorf("%s missing from %v", w, talos.Volumes)
		}
	}
	if len(talos.Ports) != 0 {
		t.Errorf("the override must not touch the published port: %v", talos.Ports)
	}
}

// Compose variables must be read and change only their target.
func TestComposeEnvironmentIsReadOneVariableAtATime(t *testing.T) {
	talos := readCompose(t, "../../docker-compose.yml").Services["talos"]
	base := config.Default()
	for name, check := range map[string]func(c config.Config) bool{
		"TALOS_LLM_BASE_URL": func(c config.Config) bool {
			return c.LLM.BaseURL == "http://x:1" && c.Embed.BaseURL == base.Embed.BaseURL
		},
		"TALOS_EMBED_BASE_URL": func(c config.Config) bool {
			return c.Embed.BaseURL == "http://x:1" && c.LLM.BaseURL == base.LLM.BaseURL
		},
	} {
		if _, ok := talos.Environment[name]; !ok {
			t.Errorf("compose does not set %s", name)
		}
		c := config.Default()
		c.ApplyEnv(func(k string) string {
			if k == name {
				return "http://x:1"
			}
			return ""
		})
		if !check(c) {
			t.Errorf("%s does not change exactly its own setting: %+v %+v", name, c.LLM, c.Embed)
		}
	}
}

var dockerInstr = regexp.MustCompile(`^([A-Z]+)\s+(.*)$`)

// dockerfileInstructions joins continuation lines; drops comments.
func dockerfileInstructions(t *testing.T) map[string][]string {
	t.Helper()
	b, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	var cur string
	for _, l := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "#") || trimmed == "" {
			continue
		}
		if strings.HasSuffix(trimmed, "\\") {
			cur += strings.TrimSuffix(trimmed, "\\") + " "
			continue
		}
		lines = append(lines, cur+trimmed)
		cur = ""
	}
	out := map[string][]string{}
	for _, l := range lines {
		if m := dockerInstr.FindStringSubmatch(l); m != nil {
			out[m[1]] = append(out[m[1]], m[2])
		}
	}
	return out
}

func TestDockerfileInstructions(t *testing.T) {
	d := dockerfileInstructions(t)
	one := func(name, want string) {
		t.Helper()
		if len(d[name]) != 1 || d[name][0] != want {
			t.Errorf("%s: got %q, want exactly %q", name, d[name], want)
		}
	}
	one("EXPOSE", "8080")
	one("ENTRYPOINT", `["/app/talos", "-c", "/etc/talos/talos.json"]`)
	one("CMD", `["serve"]`)
	if len(d["HEALTHCHECK"]) != 1 || !strings.Contains(d["HEALTHCHECK"][0], `CMD ["/app/talos", "-c", "/etc/talos/talos.json", "healthcheck"]`) {
		t.Errorf("HEALTHCHECK: %q", d["HEALTHCHECK"])
	}
	if len(d["VOLUME"]) != 0 {
		t.Errorf("a VOLUME instruction leaves anonymous volumes behind every `docker run`: %q", d["VOLUME"])
	}
	if len(d["USER"]) != 0 {
		t.Errorf("USER is set by the :nonroot base image, not overridden: %q", d["USER"])
	}
	froms := d["FROM"]
	if len(froms) != 2 || !strings.Contains(froms[0], "golang:") || !strings.Contains(froms[1], "distroless/static") || !strings.Contains(froms[1], ":nonroot") {
		t.Errorf("stages: %q", froms)
	}
	copies := strings.Join(d["COPY"], "\n")
	for _, want := range []string{"docker/talos.json /etc/talos/talos.json", "/out/talos /app/talos", "/out/ui /app/ui", "--chown=65532:65532 /out/data /data", "--chown=65532:65532 /out/pdfs /pdfs"} {
		if !strings.Contains(copies, want) {
			t.Errorf("no COPY %s", want)
		}
	}
	if len(d["ADD"]) != 0 {
		t.Errorf("ADD can fetch remote URLs: %q", d["ADD"])
	}
	// the secret is only ever mounted, never copied or echoed
	for _, run := range d["RUN"] {
		if strings.Contains(run, "cacert") && !strings.Contains(run, "--mount=type=secret,id=cacert,required=false") {
			t.Errorf("cacert used without the secret mount: %q", run)
		}
	}
}

func slices_contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
