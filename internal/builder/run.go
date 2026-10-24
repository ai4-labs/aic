package builder

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/rakyll/ai-container/internal/dockerfile"
	"github.com/rakyll/ai-container/internal/executor"
	"github.com/rakyll/ai-container/internal/rootfs"
)

// runSkip lists paths the container runtime manages. Changes to them
// during RUN are not committed, matching Docker.
var runSkip = map[string]bool{
	"proc":              true,
	"sys":               true,
	"dev":               true,
	"etc/hosts":         true,
	"etc/hostname":      true,
	"etc/resolv.conf":   true,
	"run/.containerenv": true,
	"run/secrets":       true,
}

func skipRunPath(p string) bool { return runSkip[p] }

func (b *builder) executor() (executor.Executor, error) {
	if b.exec != nil {
		return b.exec, nil
	}
	if b.opts.NewExecutor == nil {
		return nil, executor.ErrUnavailable
	}
	e, err := b.opts.NewExecutor()
	if err != nil {
		return nil, err
	}
	b.exec = e
	return e, nil
}

func (b *builder) run(ctx context.Context, s *stage, ins *dockerfile.Instruction) error {
	network := "default"
	for _, f := range ins.Flags {
		k, v, _ := strings.Cut(strings.TrimPrefix(f, "--"), "=")
		switch k {
		case "network":
			if v != "default" && v != "none" && v != "host" {
				return fmt.Errorf("--network=%s: must be default, none or host", v)
			}
			network = v
		default:
			return fmt.Errorf("--%s is not supported", k)
		}
	}
	argv := ins.Args
	if !ins.JSON {
		argv = append(append([]string{}, s.shell()...), ins.Args[0])
	}
	exe, err := b.executor()
	if err != nil {
		return err
	}
	if err := b.ensureTree(s); err != nil {
		return err
	}
	img, err := s.image(b.created)
	if err != nil {
		return err
	}

	export, err := os.CreateTemp(b.tmp, "rootfs-*.tar")
	if err != nil {
		return err
	}
	defer func() {
		export.Close()
		os.Remove(export.Name())
	}()
	err = exe.Run(ctx, executor.Spec{
		Image:   img,
		Args:    argv,
		Env:     s.runEnv(),
		WorkDir: s.config.WorkingDir,
		User:    s.config.User,
		Network: network,
		Stdout:  b.opts.Stdout,
		Stderr:  b.opts.Stderr,
	}, export)
	var ee *executor.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("command %q exited with status %d", strings.Join(argv, " "), ee.Code)
	}
	if err != nil {
		return err
	}

	// Diff the container's filesystem against the image.
	if _, err := export.Seek(0, io.SeekStart); err != nil {
		return err
	}
	next := rootfs.New()
	if err := next.Apply(export, -1); err != nil {
		return fmt.Errorf("reading container filesystem: %w", err)
	}
	changed, deleted := rootfs.Diff(s.tree, next, skipRunPath)
	include := make(map[string]bool, len(changed))
	for _, p := range changed {
		include[p] = true
	}
	// A hard link needs its target in the same layer.
	for _, p := range changed {
		if e, _ := next.Get(p); e.Type == rootfs.TypeLink {
			include[e.Linkname] = true
		}
	}

	lw, err := b.newLayer(s)
	if err != nil {
		return err
	}
	if err := writeDiff(lw, export, include, deleted); err != nil {
		lw.abort()
		return err
	}
	return b.commit(s, lw, ins)
}

func writeDiff(lw *layerWriter, export *os.File, include map[string]bool, deleted []string) error {
	for _, p := range deleted {
		if err := lw.whiteout(p); err != nil {
			return err
		}
	}
	if _, err := export.Seek(0, io.SeekStart); err != nil {
		return err
	}
	tr := tar.NewReader(export)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		p, err := rootfs.CleanName(hdr.Name)
		if err != nil {
			return err
		}
		if !include[p] {
			continue
		}
		e, ok, err := rootfs.EntryFromHeader(hdr)
		if err != nil || !ok {
			return err
		}
		if err := lw.write(e.Header(p), tr); err != nil {
			return err
		}
	}
}
