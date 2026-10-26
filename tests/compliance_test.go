//go:build compliance

// Package compliance builds the same Dockerfiles with docker and aic, runs
// the same commands in both, and reports where aic's behaviour differs.
//
//	go test -tags compliance ./tests/ -v
//
// It needs a running Docker daemon and an aic binary (AIC, ../aic, or PATH).
// Images are built from a private store (AIC_STORE is a temp dir) and the
// docker images are removed afterwards. See README.md for the results.
package compliance

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
)

var (
	dockerBin, aicBin string
	aicStore          string
	dockerImages      []string // built by docker, removed in TestMain
	runID             = fmt.Sprintf("aic-compliance-%d", os.Getpid())
)

func TestMain(m *testing.M) {
	var err error
	if dockerBin, err = findDocker(); err != nil {
		fmt.Fprintln(os.Stderr, "skipping:", err)
		return
	}
	if aicBin, err = findAic(); err != nil {
		fmt.Fprintln(os.Stderr, "skipping:", err)
		return
	}
	if aicStore, err = os.MkdirTemp("", "aic-compliance-store"); err != nil {
		panic(err)
	}
	code := m.Run()
	if len(dockerImages) > 0 {
		exec.Command(dockerBin, append([]string{"rmi", "-f"}, dockerImages...)...).Run()
	}
	os.RemoveAll(aicStore)
	os.Exit(code)
}

func findDocker() (string, error) {
	p, err := exec.LookPath("docker")
	if err != nil {
		p = "/Applications/Docker.app/Contents/Resources/bin/docker"
		if _, err := os.Stat(p); err != nil {
			return "", errors.New("docker not found")
		}
	}
	if out, err := exec.Command(p, "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		return "", fmt.Errorf("docker daemon not available: %s", strings.TrimSpace(string(out)))
	}
	return p, nil
}

func findAic() (string, error) {
	if p := os.Getenv("AIC"); p != "" {
		return filepath.Abs(p)
	}
	if _, err := os.Stat("../aic"); err == nil {
		return filepath.Abs("../aic")
	}
	if p, err := exec.LookPath("aic"); err == nil {
		return p, nil
	}
	return "", errors.New("aic not found: set AIC or run make in the repository")
}

// A file in the build context.
type file struct {
	data    string
	mode    os.FileMode // 0 means 0644
	symlink string      // if set, a symlink to this target
}

// A command to run in the image with docker run and aic run.
type run struct {
	args    []string
	env     []string // -e flags
	user    string   // -u
	workdir string   // -w / --workdir
	stdin   string   // connected with -i when not empty
	hostEnv []string // KEY=VALUE set in the process environment (for -e KEY)
	stderr  bool     // also compare stderr
}

// A compliance case: a Dockerfile built by both tools, then compared.
type tc struct {
	name       string
	dockerfile string
	files      map[string]file
	buildArgs  []string
	target     string
	skipConfig bool  // don't compare the image config
	buildFails bool  // both builds must fail
	runs       []run // commands compared after a successful build
	note       string
}

type buildResult struct {
	id  string // image ID / digest
	ref string // name to run
	cfg v1.Config
	err error
	out string
}

func (c tc) context(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(c.dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, f := range c.files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if f.symlink != "" {
			if err := os.Symlink(f.symlink, p); err != nil {
				t.Fatal(err)
			}
			continue
		}
		mode := f.mode
		if mode == 0 {
			mode = 0o644
		}
		if err := os.WriteFile(p, []byte(f.data), mode); err != nil {
			t.Fatal(err)
		}
		os.Chmod(p, mode) // umask
	}
	return dir
}

func command(ctx context.Context, env []string, bin string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), env...)
	return cmd
}

func (c tc) buildDocker(t *testing.T, ctxDir string) buildResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tag := runID + "-" + strings.ToLower(c.name)
	args := []string{"build", "--pull", "-q", "-t", tag}
	for _, a := range c.buildArgs {
		args = append(args, "--build-arg", a)
	}
	if c.target != "" {
		args = append(args, "--target", c.target)
	}
	args = append(args, ctxDir)
	out, err := command(ctx, nil, dockerBin, args...).CombinedOutput()
	r := buildResult{out: string(out), err: err}
	if err != nil {
		return r
	}
	r.id = strings.TrimSpace(string(out))
	r.ref = tag
	dockerImages = append(dockerImages, tag)
	js, err := exec.Command(dockerBin, "image", "inspect", "--format", "{{json .Config}}", r.id).Output()
	if err != nil {
		t.Fatalf("docker inspect: %v", err)
	}
	if err := json.Unmarshal(js, &r.cfg); err != nil {
		t.Fatalf("docker inspect: %v", err)
	}
	return r
}

func (c tc) buildAic(t *testing.T, ctxDir string) buildResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tag := runID + "-" + strings.ToLower(c.name) + ":1"
	args := []string{"build", "-q", "-t", tag}
	for _, a := range c.buildArgs {
		args = append(args, "--build-arg", a)
	}
	if c.target != "" {
		args = append(args, "--target", c.target)
	}
	args = append(args, ctxDir)
	var stdout, stderr bytes.Buffer
	cmd := command(ctx, []string{"AIC_STORE=" + aicStore}, aicBin, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	r := buildResult{out: stdout.String() + stderr.String(), err: err}
	if err != nil {
		return r
	}
	r.id = strings.TrimSpace(stdout.String())
	r.ref = tag
	h, err := v1.NewHash(r.id)
	if err != nil {
		t.Fatalf("aic build -q printed %q: %v", r.id, err)
	}
	lp, err := layout.FromPath(aicStore)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := lp.ImageIndex()
	if err != nil {
		t.Fatal(err)
	}
	img, err := idx.Image(h)
	if err != nil {
		t.Fatalf("image %s not in store: %v", h, err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	r.cfg = cf.Config
	return r
}

type runResult struct {
	stdout, stderr string
	code           int
}

func runDocker(t *testing.T, ref string, r run) runResult {
	t.Helper()
	args := []string{"run", "--rm", "--network", "none"}
	if r.stdin != "" {
		args = append(args, "-i")
	}
	for _, e := range r.env {
		args = append(args, "-e", e)
	}
	if r.user != "" {
		args = append(args, "-u", r.user)
	}
	if r.workdir != "" {
		args = append(args, "-w", r.workdir)
	}
	args = append(append(args, ref), r.args...)
	return execRun(t, r, dockerBin, args)
}

func runAic(t *testing.T, ref string, r run) runResult {
	t.Helper()
	args := []string{"run"}
	if r.stdin != "" {
		args = append(args, "-i")
	}
	for _, e := range r.env {
		args = append(args, "-e", e)
	}
	if r.user != "" {
		args = append(args, "-u", r.user)
	}
	if r.workdir != "" {
		args = append(args, "--workdir", r.workdir)
	}
	args = append(append(args, ref), r.args...)
	r.hostEnv = append(r.hostEnv, "AIC_STORE="+aicStore)
	return execRun(t, r, aicBin, args)
}

func execRun(t *testing.T, r run, bin string, args []string) runResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := command(ctx, r.hostEnv, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if r.stdin != "" {
		cmd.Stdin = strings.NewReader(r.stdin)
	}
	err := cmd.Run()
	res := runResult{stdout: stdout.String(), stderr: stderr.String()}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("%s %v: %v", filepath.Base(bin), args, err)
	}
	return res
}

// normalize makes empty and nil collections equal and sorts what is unordered.
func normalize(c v1.Config) v1.Config {
	if len(c.Env) == 0 {
		c.Env = nil
	}
	if len(c.Cmd) == 0 {
		c.Cmd = nil
	}
	if len(c.Entrypoint) == 0 {
		c.Entrypoint = nil
	}
	if len(c.Shell) == 0 {
		c.Shell = nil
	}
	if len(c.OnBuild) == 0 {
		c.OnBuild = nil
	}
	if len(c.Labels) == 0 {
		c.Labels = nil
	}
	if len(c.ExposedPorts) == 0 {
		c.ExposedPorts = nil
	}
	if len(c.Volumes) == 0 {
		c.Volumes = nil
	}
	if c.Healthcheck != nil && len(c.Healthcheck.Test) == 0 && c.Healthcheck.Interval == 0 &&
		c.Healthcheck.Timeout == 0 && c.Healthcheck.StartPeriod == 0 && c.Healthcheck.Retries == 0 {
		c.Healthcheck = nil
	}
	// Only the fields a Dockerfile sets are compared.
	return v1.Config{
		Env: c.Env, Cmd: c.Cmd, Entrypoint: c.Entrypoint, Shell: c.Shell, OnBuild: c.OnBuild,
		Labels: c.Labels, ExposedPorts: c.ExposedPorts, Volumes: c.Volumes,
		WorkingDir: c.WorkingDir, User: c.User, StopSignal: c.StopSignal, Healthcheck: c.Healthcheck,
	}
}

func sorted(s []string) []string {
	s = append([]string(nil), s...)
	sort.Strings(s)
	return s
}

func jsonString(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

// diff shows the lines that differ between two texts.
func diff(name, docker, aic string) string {
	if docker == aic {
		return ""
	}
	dl, al := strings.Split(docker, "\n"), strings.Split(aic, "\n")
	seen := map[string]int{}
	for _, l := range dl {
		seen[l]++
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s differs:\n", name)
	for _, l := range al {
		if seen[l] > 0 {
			seen[l]--
		} else {
			fmt.Fprintf(&b, "  aic only:    %q\n", l)
		}
	}
	for l, n := range seen {
		for ; n > 0; n-- {
			fmt.Fprintf(&b, "  docker only: %q\n", l)
		}
	}
	return b.String()
}

func TestCompliance(t *testing.T) {
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctxDir := c.context(t)
			d := c.buildDocker(t, ctxDir)
			a := c.buildAic(t, ctxDir)
			if c.buildFails {
				if d.err == nil {
					t.Errorf("docker build succeeded, expected failure")
				}
				if a.err == nil {
					t.Errorf("aic build succeeded, expected failure")
				}
				return
			}
			if d.err != nil {
				t.Fatalf("docker build failed:\n%s", d.out)
			}
			if a.err != nil {
				t.Fatalf("aic build failed:\n%s", a.out)
			}
			if !c.skipConfig {
				t.Run("config", func(t *testing.T) {
					dc, ac := normalize(d.cfg), normalize(a.cfg)
					if reflect.DeepEqual(dc, ac) {
						return
					}
					if sd, sa := sorted(dc.Env), sorted(ac.Env); !reflect.DeepEqual(sd, sa) || dc.Env == nil {
						t.Errorf("image config differs\n%s", diff("config", jsonString(dc), jsonString(ac)))
						return
					}
					dc.Env, ac.Env = nil, nil
					if reflect.DeepEqual(dc, ac) {
						t.Errorf("Env order differs:\n  docker: %q\n  aic:    %q", d.cfg.Env, a.cfg.Env)
					} else {
						t.Errorf("image config differs\n%s", diff("config", jsonString(dc), jsonString(ac)))
					}
				})
			}
			for i, r := range c.runs {
				t.Run(fmt.Sprintf("run%d", i), func(t *testing.T) {
					dr := runDocker(t, d.ref, r)
					ar := runAic(t, a.ref, r)
					var problems []string
					if dr.code != ar.code {
						problems = append(problems, fmt.Sprintf("exit status: docker %d, aic %d", dr.code, ar.code))
					}
					if s := diff("stdout", dr.stdout, ar.stdout); s != "" {
						problems = append(problems, s)
					}
					if r.stderr {
						if s := diff("stderr", dr.stderr, ar.stderr); s != "" {
							problems = append(problems, s)
						}
					}
					if len(problems) > 0 {
						t.Errorf("%v\n%s\n(docker stderr: %q; aic stderr: %q)", r.args, strings.Join(problems, "\n"), dr.stderr, ar.stderr)
					}
				})
			}
		})
	}
}

// listing prints every path under dir with its mode, owner, type, link
// target and content hash, in a stable order. Sizes and times are left out:
// directory sizes depend on the filesystem.
func listing(dir string) []string {
	return []string{"sh", "-c", `cd "$1" && find . -print | LC_ALL=C sort | while read -r f; do
  stat -c '%n %A %u:%g %F' "$f"
  if [ -L "$f" ]; then echo "  -> $(readlink "$f")"; elif [ -f "$f" ]; then echo "  $(md5sum < "$f" | cut -d' ' -f1)"; fi
done`, "sh", dir}
}

func sh(script string) []string { return []string{"sh", "-c", script} }

// targz builds a .tar.gz archive of the given files for ADD tests.
func targz(files map[string]string) string {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		data := files[n]
		hdr := &tar.Header{Name: n, Mode: 0o644, Size: int64(len(data)), ModTime: time.Unix(0, 0)}
		if strings.HasSuffix(n, "/") {
			hdr.Typeflag, hdr.Mode, hdr.Size = tar.TypeDir, 0o755, 0
		}
		tw.WriteHeader(hdr)
		tw.Write([]byte(data))
	}
	tw.Close()
	gz.Close()
	return buf.String()
}
