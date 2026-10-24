// Package builder builds OCI images from Dockerfiles.
//
// Images are assembled natively: base images come from the local store or
// a registry, COPY/ADD layers are written directly from the build context,
// and metadata instructions edit the image config. Only RUN needs a
// container; it is delegated to an executor.Executor, and the resulting
// filesystem is diffed against the image to produce the layer.
package builder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/rakyll/ai-container/internal/dockerfile"
	"github.com/rakyll/ai-container/internal/executor"
	"github.com/rakyll/ai-container/internal/rootfs"
	"github.com/rakyll/ai-container/internal/store"
)

// Options configures a build.
type Options struct {
	// ContextDir is the build context directory.
	ContextDir string
	// Dockerfile is the path to the Dockerfile. Default: ContextDir/Dockerfile.
	Dockerfile string
	// Tags name the resulting image in the store.
	Tags []name.Reference
	// BuildArgs override ARG defaults.
	BuildArgs map[string]string
	// Target is the stage to build. Default: the last stage.
	Target string
	// Platform is the target platform. Default: linux on the host's architecture.
	Platform v1.Platform
	// Pull always pulls base images instead of using the local store.
	Pull bool
	// Created is the image creation time. Default: now.
	Created time.Time

	Store *store.Store
	// NewExecutor is called the first time a RUN instruction executes.
	NewExecutor func() (executor.Executor, error)

	// Progress receives step messages; Stdout and Stderr receive the
	// output of RUN commands.
	Progress, Stdout, Stderr io.Writer
}

// DefaultPlatform is linux on the host's CPU architecture.
func DefaultPlatform() v1.Platform {
	return v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
}

var defaultShell = []string{"/bin/sh", "-c"}

type builder struct {
	opts    Options
	esc     rune
	tmp     string
	created v1.Time

	ctxSrc *contextSource
	exec   executor.Executor

	globals  map[string]*string // ARGs declared before the first FROM
	auto     map[string]string  // automatic platform ARGs
	usedArgs map[string]bool

	defs   []stageDef
	stages []*stage          // by index; nil for skipped stages
	images map[string]*stage // COPY --from=<image> cache
}

type stageDef struct {
	index int
	name  string
	from  dockerfile.Instruction
	body  []dockerfile.Instruction
}

// stage is the state of one build stage.
type stage struct {
	name     string
	index    int
	platform v1.Platform
	base     v1.Image
	layers   []v1.Layer // all layers, base first; indexes match tree entries
	adds     []mutate.Addendum
	layerMT  types.MediaType
	config   v1.Config
	author   string
	tree     *rootfs.Tree // built on first use
	args     map[string]*string
	cmdSet   bool
}

// Build builds the Dockerfile and stores the result under opts.Tags.
func Build(ctx context.Context, opts Options) (v1.Image, error) {
	if opts.Store == nil {
		return nil, errors.New("builder: no store")
	}
	if opts.Dockerfile == "" {
		opts.Dockerfile = filepath.Join(opts.ContextDir, "Dockerfile")
	}
	if opts.Platform.OS == "" {
		opts.Platform = DefaultPlatform()
	}
	if opts.Created.IsZero() {
		opts.Created = time.Now()
	}
	for _, w := range []*io.Writer{&opts.Progress, &opts.Stdout, &opts.Stderr} {
		if *w == nil {
			*w = io.Discard
		}
	}

	f, err := os.Open(opts.Dockerfile)
	if err != nil {
		return nil, err
	}
	df, err := dockerfile.Parse(f)
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", opts.Dockerfile, err)
	}

	ctxSrc, err := openContext(opts.ContextDir, opts.Dockerfile)
	if err != nil {
		return nil, err
	}
	defer ctxSrc.root.Close()

	tmp, err := os.MkdirTemp("", "aic-build-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	b := &builder{
		opts:     opts,
		esc:      df.Escape,
		tmp:      tmp,
		created:  v1.Time{Time: opts.Created.UTC().Truncate(time.Second)},
		ctxSrc:   ctxSrc,
		globals:  map[string]*string{},
		usedArgs: map[string]bool{},
		images:   map[string]*stage{},
	}
	b.auto = autoArgs(opts.Platform)
	img, err := b.build(ctx, df)
	if err != nil {
		return nil, err
	}
	var unused []string
	for k := range opts.BuildArgs {
		if !b.usedArgs[k] {
			unused = append(unused, k)
		}
	}
	if len(unused) > 0 {
		fmt.Fprintf(opts.Progress, "[Warning] build args not consumed by the Dockerfile: %s\n", strings.Join(unused, ", "))
	}
	if err := opts.Store.Put(img, opts.Tags...); err != nil {
		return nil, fmt.Errorf("storing image: %w", err)
	}
	// Return the stored copy: the built image's layers live in tmp.
	d, err := img.Digest()
	if err != nil {
		return nil, err
	}
	return opts.Store.Image(d.String())
}

func platformString(p v1.Platform) string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

func autoArgs(target v1.Platform) map[string]string {
	// RUN executes in a Linux container on the host's architecture.
	build := DefaultPlatform()
	return map[string]string{
		"TARGETPLATFORM": platformString(target),
		"TARGETOS":       target.OS,
		"TARGETARCH":     target.Architecture,
		"TARGETVARIANT":  target.Variant,
		"BUILDPLATFORM":  platformString(build),
		"BUILDOS":        build.OS,
		"BUILDARCH":      build.Architecture,
		"BUILDVARIANT":   build.Variant,
	}
}

func (b *builder) build(ctx context.Context, df *dockerfile.Dockerfile) (v1.Image, error) {
	// Split into global ARGs and stages.
	for _, ins := range df.Instructions {
		switch {
		case ins.Cmd == "from":
			def := stageDef{index: len(b.defs), from: ins}
			if len(ins.Args) == 3 {
				def.name = strings.ToLower(ins.Args[2])
			}
			b.defs = append(b.defs, def)
		case len(b.defs) == 0:
			if ins.Cmd != "arg" {
				return nil, fmt.Errorf("line %d: %s before the first FROM", ins.Line, strings.ToUpper(ins.Cmd))
			}
			if err := b.declareGlobalArgs(&ins); err != nil {
				return nil, err
			}
		default:
			d := &b.defs[len(b.defs)-1]
			d.body = append(d.body, ins)
		}
	}
	if len(b.defs) == 0 {
		return nil, errors.New("the Dockerfile has no FROM instruction")
	}

	target := len(b.defs) - 1
	if b.opts.Target != "" {
		target = -1
		for _, d := range b.defs {
			if d.name == strings.ToLower(b.opts.Target) {
				target = d.index
			}
		}
		if target < 0 {
			return nil, fmt.Errorf("target stage %q not found", b.opts.Target)
		}
	}
	needed, err := b.neededStages(target)
	if err != nil {
		return nil, err
	}

	b.stages = make([]*stage, len(b.defs))
	for _, d := range b.defs[:target+1] {
		if !needed[d.index] {
			continue
		}
		s, err := b.runStage(ctx, d)
		if err != nil {
			return nil, err
		}
		b.stages[d.index] = s
	}
	return b.stages[target].image(b.created)
}

// neededStages returns the stages the target depends on, through FROM
// and COPY --from. A --from containing a variable makes every earlier
// stage needed, since it can't be resolved before building.
func (b *builder) neededStages(target int) (map[int]bool, error) {
	byName := map[string]int{}
	for _, d := range b.defs {
		if d.name != "" {
			byName[d.name] = d.index
		}
	}
	refers := func(d stageDef, ref string) (int, bool) {
		if i, ok := byName[strings.ToLower(ref)]; ok && i < d.index {
			return i, true
		}
		if i, err := strconv.Atoi(ref); err == nil && i >= 0 && i < d.index {
			return i, true
		}
		return 0, false
	}
	needed := map[int]bool{}
	var visit func(i int) error
	visit = func(i int) error {
		if needed[i] {
			return nil
		}
		needed[i] = true
		d := b.defs[i]
		base, err := dockerfile.ProcessWord(d.from.Args[0], b.globalLookup, b.esc)
		if err != nil {
			return fmt.Errorf("line %d: %w", d.from.Line, err)
		}
		var deps []int
		if j, ok := refers(d, base); ok {
			deps = append(deps, j)
		}
		for _, ins := range d.body {
			from, ok := ins.Flag("from")
			if !ok || (ins.Cmd != "copy" && ins.Cmd != "add") {
				continue
			}
			if strings.Contains(from, "$") {
				for j := 0; j < i; j++ {
					deps = append(deps, j)
				}
			} else if j, ok := refers(d, from); ok {
				deps = append(deps, j)
			}
		}
		for _, j := range deps {
			if err := visit(j); err != nil {
				return err
			}
		}
		return nil
	}
	return needed, visit(target)
}

func (b *builder) globalLookup(k string) (string, bool) {
	if v, ok := b.globals[k]; ok && v != nil {
		return *v, true
	}
	if v, ok := b.auto[k]; ok {
		return v, true
	}
	return "", false
}

func (b *builder) declareGlobalArgs(ins *dockerfile.Instruction) error {
	for _, raw := range ins.Args {
		k, val, err := b.argValue(raw, b.globalLookup, nil)
		if err != nil {
			return fmt.Errorf("line %d: %w", ins.Line, err)
		}
		b.globals[k] = val
	}
	return nil
}

// argValue evaluates an ARG token "name[=default]". inherit supplies a
// value for a bare name (global ARGs re-declared in a stage).
func (b *builder) argValue(raw string, lookup dockerfile.Lookup, inherit func(string) *string) (string, *string, error) {
	k, def, hasDef := strings.Cut(raw, "=")
	if k == "" || strings.ContainsAny(k, " \t$") {
		return "", nil, fmt.Errorf("ARG: invalid name %q", k)
	}
	var val *string
	if hasDef {
		v, err := dockerfile.ProcessWord(def, lookup, b.esc)
		if err != nil {
			return "", nil, err
		}
		val = &v
	} else if inherit != nil {
		val = inherit(k)
	}
	if v, ok := b.opts.BuildArgs[k]; ok {
		val = &v
		b.usedArgs[k] = true
	}
	return k, val, nil
}

func (b *builder) say(format string, a ...any) {
	fmt.Fprintf(b.opts.Progress, format+"\n", a...)
}

func (b *builder) runStage(ctx context.Context, d stageDef) (*stage, error) {
	total := len(d.body) + 1
	prefix := ""
	if len(b.defs) > 1 {
		prefix = fmt.Sprintf("[%d/%d] ", d.index+1, len(b.defs))
	}
	b.say("%sSTEP 1/%d: %s", prefix, total, d.from.Original)
	s, err := b.from(ctx, d)
	if err != nil {
		return nil, fmt.Errorf("line %d: %w", d.from.Line, err)
	}
	for i := range d.body {
		ins := &d.body[i]
		b.say("%sSTEP %d/%d: %s", prefix, i+2, total, ins.Original)
		if err := b.dispatch(ctx, s, ins); err != nil {
			return nil, fmt.Errorf("line %d: %s: %w", ins.Line, strings.ToUpper(ins.Cmd), err)
		}
	}
	return s, nil
}

func (b *builder) from(ctx context.Context, d stageDef) (*stage, error) {
	ins := d.from
	for _, f := range ins.Flags {
		if !strings.HasPrefix(f, "--platform=") {
			return nil, fmt.Errorf("unsupported flag %s", f)
		}
	}
	ref, err := dockerfile.ProcessWord(ins.Args[0], b.globalLookup, b.esc)
	if err != nil {
		return nil, err
	}
	platform := b.opts.Platform
	if p, ok := ins.Flag("platform"); ok {
		pe, err := dockerfile.ProcessWord(p, b.globalLookup, b.esc)
		if err != nil {
			return nil, err
		}
		pp, err := v1.ParsePlatform(pe)
		if err != nil {
			return nil, err
		}
		platform = *pp
	}

	s := &stage{name: d.name, index: d.index, platform: platform, args: map[string]*string{}}
	if strings.EqualFold(ref, "scratch") {
		s.base = mutate.ConfigMediaType(mutate.MediaType(empty.Image, types.OCIManifestSchema1), types.OCIConfigJSON)
		s.layerMT = types.OCILayer
		return s, nil
	}
	for _, prev := range b.stages[:d.index] {
		if prev != nil && prev.name != "" && prev.name == strings.ToLower(ref) {
			s.inherit(prev)
			return s, nil
		}
	}
	img, err := b.baseImage(ctx, ref, platform)
	if err != nil {
		return nil, err
	}
	if err := s.setBase(img); err != nil {
		return nil, err
	}
	if len(s.config.OnBuild) > 0 {
		b.say("[Warning] ignoring %d ONBUILD trigger(s) from %s", len(s.config.OnBuild), ref)
		s.config.OnBuild = nil
	}
	return s, nil
}

func (s *stage) setBase(img v1.Image) error {
	cf, err := img.ConfigFile()
	if err != nil {
		return err
	}
	layers, err := img.Layers()
	if err != nil {
		return err
	}
	mt, err := img.MediaType()
	if err != nil {
		return err
	}
	s.base = img
	s.layers = layers
	s.config = cf.DeepCopy().Config
	s.layerMT = types.OCILayer
	if mt == types.DockerManifestSchema2 {
		s.layerMT = types.DockerLayer
	}
	return nil
}

// inherit makes s continue from an earlier stage (FROM <stage>).
func (s *stage) inherit(prev *stage) {
	s.base = prev.base
	s.layers = append([]v1.Layer(nil), prev.layers...)
	s.adds = append([]mutate.Addendum(nil), prev.adds...)
	s.layerMT = prev.layerMT
	cf := &v1.ConfigFile{Config: prev.config}
	s.config = cf.DeepCopy().Config
	s.author = prev.author
	if prev.tree != nil {
		s.tree = prev.tree.Clone()
	}
}

// DisplayName shortens Docker Hub names for messages.
func DisplayName(ref name.Reference) string {
	return strings.Replace(ref.Name(), name.DefaultRegistry+"/", "docker.io/", 1)
}

// baseImage returns ref for platform, from the store unless missing or
// --pull was given.
func (b *builder) baseImage(ctx context.Context, ref string, platform v1.Platform) (v1.Image, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return nil, err
	}
	if !b.opts.Pull {
		if img, err := b.opts.Store.Image(r.Name()); err == nil {
			if cf, err := img.ConfigFile(); err == nil && cf.OS == platform.OS && cf.Architecture == platform.Architecture &&
				(platform.Variant == "" || cf.Variant == platform.Variant) {
				return img, nil
			}
		}
	}
	b.say("Pulling %s (%s)...", DisplayName(r), platformString(platform))
	img, err := b.opts.Store.Pull(ctx, r, platform)
	if err != nil {
		return nil, fmt.Errorf("pulling %s: %w", ref, err)
	}
	return img, nil
}

// image materializes the stage as an image.
func (s *stage) image(created v1.Time) (v1.Image, error) {
	img, err := mutate.Append(s.base, s.adds...)
	if err != nil {
		return nil, err
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	cf = cf.DeepCopy()
	cf.Config = s.config
	cf.Created = created
	cf.Author = s.author
	cf.OS = s.platform.OS
	cf.Architecture = s.platform.Architecture
	cf.Variant = s.platform.Variant
	cf.Container = ""
	cf.DockerVersion = ""
	return mutate.ConfigFile(img, cf)
}

func (b *builder) ensureTree(s *stage) error {
	if s.tree != nil {
		return nil
	}
	t := rootfs.New()
	for i, l := range s.layers {
		rc, err := l.Uncompressed()
		if err != nil {
			return err
		}
		err = t.Apply(rc, i)
		rc.Close()
		if err != nil {
			return fmt.Errorf("reading layer %d: %w", i, err)
		}
	}
	s.tree = t
	return nil
}

// lookup resolves a variable in a stage: ENV wins over ARG.
func (s *stage) lookup(k string) (string, bool) {
	if v, ok := getEnv(s.config.Env, k); ok {
		return v, true
	}
	if v, ok := s.args[k]; ok && v != nil {
		return *v, true
	}
	return "", false
}

func getEnv(env []string, k string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		if ek, v, _ := strings.Cut(env[i], "="); ek == k {
			return v, true
		}
	}
	return "", false
}

func setEnv(env []string, k, v string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if ek, _, _ := strings.Cut(kv, "="); ek != k {
			out = append(out, kv)
		}
	}
	return append(out, k+"="+v)
}

// runEnv is the environment for RUN: the image's ENV plus ARGs that ENV
// doesn't override. ARGs are not persisted in the image.
func (s *stage) runEnv() []string {
	env := append([]string(nil), s.config.Env...)
	for k, v := range s.args {
		if v == nil {
			continue
		}
		if _, ok := getEnv(s.config.Env, k); !ok {
			env = append(env, k+"="+*v)
		}
	}
	return env
}

func (s *stage) shell() []string {
	if len(s.config.Shell) > 0 {
		return s.config.Shell
	}
	return defaultShell
}

func (b *builder) word(s *stage, w string) (string, error) {
	return dockerfile.ProcessWord(w, s.lookup, b.esc)
}

func (b *builder) dispatch(ctx context.Context, s *stage, ins *dockerfile.Instruction) error {
	if !flagAllowed(ins) {
		return fmt.Errorf("unsupported flag %s", ins.Flags[0])
	}
	switch ins.Cmd {
	case "run":
		return b.run(ctx, s, ins)
	case "copy", "add":
		return b.copy(ctx, s, ins)
	case "arg":
		for _, raw := range ins.Args {
			k, val, err := b.argValue(raw, s.lookup, func(k string) *string {
				if v, ok := b.globals[k]; ok && v != nil {
					c := *v
					return &c
				}
				if v, ok := b.auto[k]; ok {
					return &v
				}
				return nil
			})
			if err != nil {
				return err
			}
			s.args[k] = val
		}
		return nil // ARG leaves no trace in the image
	case "env", "label":
		vals := make([]string, len(ins.Args))
		for i, a := range ins.Args {
			v, err := b.word(s, a) // all values see the environment before this instruction
			if err != nil {
				return err
			}
			vals[i] = v
		}
		for i := 0; i < len(vals); i += 2 {
			if ins.Cmd == "env" {
				s.config.Env = setEnv(s.config.Env, vals[i], vals[i+1])
			} else {
				if s.config.Labels == nil {
					s.config.Labels = map[string]string{}
				}
				s.config.Labels[vals[i]] = vals[i+1]
			}
		}
	case "maintainer":
		s.author = ins.Args[0]
	case "workdir":
		w, err := b.word(s, ins.Args[0])
		if err != nil {
			return err
		}
		if !path.IsAbs(w) {
			cur := s.config.WorkingDir
			if cur == "" {
				cur = "/"
			}
			w = path.Join(cur, w)
		}
		s.config.WorkingDir = path.Clean(w)
		if err := b.mkWorkdir(s, ins); err != nil {
			return err
		}
		return nil
	case "user":
		u, err := b.word(s, ins.Args[0])
		if err != nil {
			return err
		}
		s.config.User = u
	case "expose":
		for _, a := range ins.Args {
			p, err := b.word(s, a)
			if err != nil {
				return err
			}
			ports, err := expandPorts(p)
			if err != nil {
				return err
			}
			if s.config.ExposedPorts == nil {
				s.config.ExposedPorts = map[string]struct{}{}
			}
			for _, port := range ports {
				s.config.ExposedPorts[port] = struct{}{}
			}
		}
	case "volume":
		for _, a := range ins.Args {
			v, err := b.expandArg(s, ins, a)
			if err != nil {
				return err
			}
			if v == "" {
				return errors.New("empty volume path")
			}
			if s.config.Volumes == nil {
				s.config.Volumes = map[string]struct{}{}
			}
			s.config.Volumes[v] = struct{}{}
		}
	case "stopsignal":
		sig, err := b.word(s, ins.Args[0])
		if err != nil {
			return err
		}
		s.config.StopSignal = sig
	case "shell":
		s.config.Shell = append([]string(nil), ins.Args...)
	case "cmd":
		s.config.Cmd = b.commandArgs(s, ins, ins.Args)
		s.cmdSet = true
	case "entrypoint":
		s.config.Entrypoint = b.commandArgs(s, ins, ins.Args)
		if !s.cmdSet {
			s.config.Cmd = nil // an inherited CMD doesn't apply to a new ENTRYPOINT
		}
	case "healthcheck":
		hc, err := healthcheck(ins)
		if err != nil {
			return err
		}
		s.config.Healthcheck = hc
	case "onbuild":
		trigger := strings.ToUpper(strings.Fields(ins.Args[0])[0])
		if trigger == "ONBUILD" || trigger == "FROM" || trigger == "MAINTAINER" {
			return fmt.Errorf("%s is not allowed as an ONBUILD trigger", trigger)
		}
		s.config.OnBuild = append(s.config.OnBuild, ins.Args[0])
	default:
		return fmt.Errorf("unexpected instruction")
	}
	s.adds = append(s.adds, mutate.Addendum{History: b.history(ins, true)})
	return nil
}

func flagAllowed(ins *dockerfile.Instruction) bool {
	// RUN, COPY and ADD validate their own flags.
	if ins.Cmd == "run" || ins.Cmd == "copy" || ins.Cmd == "add" || ins.Cmd == "healthcheck" {
		return true
	}
	return len(ins.Flags) == 0
}

func (b *builder) history(ins *dockerfile.Instruction, empty bool) v1.History {
	return v1.History{Created: b.created, CreatedBy: ins.Original, Comment: "aic", EmptyLayer: empty}
}

func (b *builder) expandArg(s *stage, ins *dockerfile.Instruction, a string) (string, error) {
	if ins.JSON {
		return dockerfile.ExpandVars(a, s.lookup, b.esc)
	}
	return b.word(s, a)
}

func (b *builder) commandArgs(s *stage, ins *dockerfile.Instruction, args []string) []string {
	if ins.JSON {
		return append([]string{}, args...)
	}
	return append(append([]string{}, s.shell()...), args[0])
}

func expandPorts(p string) ([]string, error) {
	port, proto, _ := strings.Cut(p, "/")
	proto = strings.ToLower(proto)
	if proto == "" {
		proto = "tcp"
	}
	if proto != "tcp" && proto != "udp" && proto != "sctp" {
		return nil, fmt.Errorf("invalid protocol in %q", p)
	}
	lo, hi, isRange := strings.Cut(port, "-")
	start, err := strconv.Atoi(lo)
	if err != nil || start < 0 || start > 65535 {
		return nil, fmt.Errorf("invalid port %q", p)
	}
	end := start
	if isRange {
		if end, err = strconv.Atoi(hi); err != nil || end < start || end > 65535 {
			return nil, fmt.Errorf("invalid port range %q", p)
		}
	}
	var out []string
	for i := start; i <= end; i++ {
		out = append(out, fmt.Sprintf("%d/%s", i, proto))
	}
	return out, nil
}

func healthcheck(ins *dockerfile.Instruction) (*v1.HealthConfig, error) {
	if ins.Args[0] == "NONE" {
		return &v1.HealthConfig{Test: []string{"NONE"}}, nil
	}
	hc := &v1.HealthConfig{}
	if ins.JSON {
		hc.Test = append([]string{"CMD"}, ins.Args[1:]...)
	} else {
		hc.Test = []string{"CMD-SHELL", ins.Args[1]}
	}
	for _, f := range ins.Flags {
		k, v, _ := strings.Cut(strings.TrimPrefix(f, "--"), "=")
		var err error
		switch k {
		case "interval":
			hc.Interval, err = time.ParseDuration(v)
		case "timeout":
			hc.Timeout, err = time.ParseDuration(v)
		case "start-period":
			hc.StartPeriod, err = time.ParseDuration(v)
		case "retries":
			hc.Retries, err = strconv.Atoi(v)
		default:
			return nil, fmt.Errorf("unsupported flag --%s", k)
		}
		if err != nil {
			return nil, fmt.Errorf("--%s: %w", k, err)
		}
	}
	return hc, nil
}

// mkWorkdir creates the working directory in a new layer if it is missing.
func (b *builder) mkWorkdir(s *stage, ins *dockerfile.Instruction) error {
	if err := b.ensureTree(s); err != nil {
		return err
	}
	dir, err := s.tree.Resolve(s.config.WorkingDir, true)
	if err != nil {
		return err
	}
	if e, ok := s.tree.Get(dir); ok {
		if !e.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", s.config.WorkingDir)
		}
		s.adds = append(s.adds, mutate.Addendum{History: b.history(ins, true)})
		return nil
	}
	lw, err := b.newLayer(s)
	if err != nil {
		return err
	}
	if err := lw.mkdirAll(dir, 0, 0, b.created.Time); err != nil {
		lw.abort()
		return err
	}
	return b.commit(s, lw, ins)
}
