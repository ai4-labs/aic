//go:build compliance

package compliance

const base = "alpine:3.20"

var cases = []tc{
	{
		name: "copy-basics",
		dockerfile: `FROM ` + base + `
COPY hello.txt /app/
COPY hello.txt /app/renamed.txt
COPY dir /app/dir-copy
COPY dir/ /app/dir-contents/
COPY *.txt /app/globbed/
COPY hello.txt run.sh /app/multi/
COPY hello.txt /app/deep/new/dirs/
`,
		files: map[string]file{
			"hello.txt":    {data: "hello\n"},
			"other.txt":    {data: "other\n"},
			"run.sh":       {data: "#!/bin/sh\necho hi\n", mode: 0o755},
			"dir/a":        {data: "a"},
			"dir/sub/b":    {data: "b", mode: 0o600},
			"dir/link":     {symlink: "a"},
			"dir/dangling": {symlink: "nowhere"},
		},
		runs: []run{{args: listing("/app")}},
	},
	{
		name: "copy-chown-chmod",
		dockerfile: `FROM ` + base + `
RUN adduser -D -u 1234 app
COPY --chown=1000:1000 hello.txt /app/uid/
COPY --chown=app hello.txt /app/name/
COPY --chown=app:app dir /app/dir
COPY --chmod=600 hello.txt /app/mode600
COPY --chmod=755 --chown=1234:1234 hello.txt /app/both
`,
		files: map[string]file{
			"hello.txt": {data: "hello\n"},
			"dir/a":     {data: "a"},
			"dir/sub/b": {data: "b"},
		},
		runs: []run{{args: listing("/app")}},
	},
	{
		name: "copy-into-workdir",
		dockerfile: `FROM ` + base + `
WORKDIR /srv/app
COPY hello.txt .
COPY hello.txt sub/
COPY hello.txt ./rel.txt
ENV DEST=/by-env
COPY hello.txt $DEST/
COPY hello.txt ${DEST}/braces.txt
`,
		files: map[string]file{"hello.txt": {data: "hello\n"}},
		runs:  []run{{args: listing("/srv")}, {args: listing("/by-env")}, {args: sh("pwd")}},
	},
	{
		name: "add-archive",
		dockerfile: `FROM ` + base + `
ADD files.tar.gz /app/extracted/
ADD files.tar.gz /app/as-file.tgz
ADD plain.txt /app/
ADD --chown=1000:1000 files.tar.gz /app/owned/
`,
		files: map[string]file{
			"files.tar.gz": {data: targz(map[string]string{"x/": "", "x/one": "1", "two": "2"})},
			"plain.txt":    {data: "plain\n"},
		},
		runs: []run{{args: listing("/app")}},
	},
	{
		name: "add-url",
		dockerfile: `FROM ` + base + `
ADD https://www.ietf.org/rfc/rfc1.txt /app/
ADD https://www.ietf.org/rfc/rfc1.txt /app/renamed.txt
`,
		runs: []run{{args: listing("/app")}},
		note: "needs network; docker gives downloaded files mode 0600",
	},
	{
		name: "run-forms",
		dockerfile: `FROM ` + base + `
ENV NAME=world
RUN echo "shell $NAME" > /app-shell.txt
RUN ["sh", "-c", "echo exec $NAME > /app-exec.txt"]
RUN ["echo", "no expansion $NAME"]
RUN mkdir -p /app && mv /app-*.txt /app/
RUN echo one && \
    echo two >> /app/cont.txt
`,
		runs: []run{{args: listing("/app")}, {args: sh("cat /app/*")}},
	},
	{
		name: "run-changes-layers",
		dockerfile: `FROM ` + base + `
COPY dir /app
RUN rm /app/gone && rm -r /app/sub && echo changed > /app/kept
RUN rm -rf /app/dir-to-file && echo file > /app/dir-to-file
RUN rm /etc/motd && rm -r /etc/periodic
RUN mkdir /app/later && touch /app/later/x
RUN rm -r /app/later
RUN chmod 4755 /app/kept && mkdir -m 1777 /app/sticky && ln -s kept /app/link && ln -s /nowhere /app/dangling
RUN mkfifo /app/fifo && chown 65534:65534 /app/fifo
`,
		files: map[string]file{
			"dir/gone":          {data: "x"},
			"dir/kept":          {data: "x"},
			"dir/sub/a":         {data: "x"},
			"dir/dir-to-file/a": {data: "x"},
		},
		runs: []run{{args: listing("/app")}, {args: sh("ls /etc/motd /etc/periodic 2>&1; echo exit=$?")}},
	},
	{
		name: "run-as-user",
		dockerfile: `FROM ` + base + `
RUN adduser -D -u 1500 app && mkdir /app && chown app /app
USER app
RUN id > /app/id && touch /app/by-app && echo $HOME > /app/home
USER root
RUN touch /app/by-root
USER 1500:1500
RUN touch /app/by-uid
USER app:root
RUN touch /app/by-app-root
`,
		runs: []run{{args: listing("/app")}, {args: sh("cat /app/id /app/home; id; echo $HOME")}},
	},
	{
		name: "env",
		dockerfile: `FROM ` + base + `
ENV A=1
ENV B="two words" C=$A-x D=${A}y E=\$A
ENV LEGACY form with spaces
ENV F=${UNSET:-default} G=${A:+set}
ENV PATH=/custom/bin:$PATH
RUN mkdir /app && env | sort > /app/build-env
`,
		runs: []run{
			{args: sh(`echo "$A|$B|$C|$D|$E|$LEGACY|$F|$G|$PATH"`)},
			{args: sh("grep -v -e ^HOSTNAME= -e ^HOME= -e ^PWD= -e ^SHLVL= -e ^_= /app/build-env")},
		},
	},
	{
		name: "env-at-run",
		dockerfile: `FROM ` + base + `
ENV A=image B=image
`,
		runs: []run{
			{args: sh(`echo "$A|$B|$C"`), env: []string{"A=flag", "C=new"}},
			{args: sh(`echo "$A|$FROMHOST|$MISSING"`), env: []string{"FROMHOST", "MISSING"}, hostEnv: []string{"FROMHOST=host-value"}},
			{args: sh(`echo "$HOME|$PATH|$USER"`)},
			{args: sh(`echo "$HOME"`), user: "nobody"},
			{args: sh(`echo "$HOME"`), user: "1234"},
		},
	},
	{
		name: "arg",
		dockerfile: `ARG TAG=3.20
FROM alpine:$TAG
ARG GREETING=hello
ARG EMPTY
ARG TAG
ENV PERSISTED=$GREETING
RUN mkdir /app && echo "$GREETING/$EMPTY/$TAG" > /app/args
RUN env | sort > /app/env
`,
		runs: []run{{args: sh(`cat /app/args; echo "$GREETING|$EMPTY|$PERSISTED|$TAG"; grep -c -e ^GREETING= -e ^TAG= /app/env`)}},
	},
	{
		name: "arg-override",
		dockerfile: `FROM ` + base + `
ARG GREETING=hello
ARG OTHER=a
ENV GREETING=$GREETING OTHER=$OTHER
`,
		buildArgs: []string{"GREETING=overridden", "OTHER="},
	},
	{
		name: "arg-predefined",
		dockerfile: `FROM ` + base + `
ARG TARGETOS TARGETARCH TARGETPLATFORM BUILDPLATFORM
ENV T=$TARGETOS/$TARGETARCH P=$TARGETPLATFORM
RUN mkdir /app && echo "$BUILDPLATFORM" > /app/buildplatform
`,
		runs: []run{{args: sh("cat /app/buildplatform")}},
	},
	{
		name: "workdir",
		dockerfile: `FROM ` + base + `
WORKDIR /a
WORKDIR b/c
RUN pwd > here
WORKDIR /x
ENV SUB=y
WORKDIR $SUB
WORKDIR ../z
RUN pwd > here
WORKDIR /final
`,
		runs: []run{
			{args: listing("/a")}, {args: listing("/x")},
			{args: sh("pwd; cat /a/b/c/here /x/z/here")},
			{args: sh("pwd"), workdir: "/tmp"},
			{args: sh("pwd"), workdir: "/new/dir"},
		},
	},
	{
		name: "cmd-entrypoint",
		dockerfile: `FROM ` + base + `
ENTRYPOINT ["echo", "entry"]
CMD ["default", "cmd"]
`,
		runs: []run{{args: nil}, {args: []string{"override"}}, {args: []string{"sh", "-c", "echo not-a-shell"}}},
	},
	{
		name: "cmd-shell-form",
		dockerfile: `FROM ` + base + `
ENV NAME=world
CMD echo "shell cmd $NAME"
`,
		runs: []run{{args: nil}, {args: []string{"echo", "override"}}},
	},
	{
		name: "entrypoint-shell-form",
		dockerfile: `FROM ` + base + `
ENTRYPOINT echo "shell entry $0"
CMD ["ignored"]
`,
		runs: []run{{args: nil}, {args: []string{"also-ignored"}}},
	},
	{
		name: "entrypoint-resets-cmd",
		dockerfile: `FROM ` + base + `
CMD ["from-stage"]
ENTRYPOINT ["echo"]
`,
		runs: []run{{args: nil}, {args: []string{"x"}}},
		note: "docker clears CMD when ENTRYPOINT is set in the same stage",
	},
	{
		name:       "cmd-inherited",
		dockerfile: `FROM ` + base + "\nLABEL only=label\n",
		runs:       []run{{args: nil}},
	},
	{
		name: "shell",
		dockerfile: `FROM ` + base + `
SHELL ["/bin/busybox", "sh", "-c"]
RUN mkdir /app && echo "$0" > /app/shell
CMD echo via-custom-shell
ENTRYPOINT echo "ep $0"
`,
		runs: []run{{args: nil}, {args: sh("cat /app/shell")}},
	},
	{
		name: "metadata",
		dockerfile: `FROM ` + base + `
LABEL a=1 b="two words" multi.line="x \
y"
LABEL legacy value with spaces
LABEL "quoted.key"='single'
EXPOSE 80 443/udp 8000-8002 9000/tcp
VOLUME ["/data", "/logs"]
VOLUME /cache /tmp/x
STOPSIGNAL SIGQUIT
HEALTHCHECK --interval=30s --timeout=5s --start-period=1s --retries=3 CMD wget -q -O- http://localhost/ || exit 1
ONBUILD RUN echo onbuild
ONBUILD COPY . /src
MAINTAINER someone
`,
	},
	{
		name: "healthcheck-forms",
		dockerfile: `FROM ` + base + `
HEALTHCHECK CMD ["wget", "-q", "http://localhost/"]
HEALTHCHECK NONE
`,
	},
	{
		name: "stopsignal-number",
		dockerfile: `FROM ` + base + `
STOPSIGNAL 9
HEALTHCHECK --start-interval=2s --interval=1m CMD true
`,
	},
	{
		name: "multistage",
		dockerfile: `FROM ` + base + ` AS build
RUN mkdir /out && echo built > /out/artifact && chmod 755 /out/artifact
FROM ` + base + ` AS second
COPY --from=build /out/artifact /app/from-named
COPY --from=0 /out/ /app/from-index/
COPY --from=` + base + ` /etc/alpine-release /app/from-image
FROM second
COPY --from=build --chown=65534:65534 /out/artifact /app/chowned
ENV FINAL=1
`,
		runs: []run{{args: listing("/app")}},
	},
	{
		name: "multistage-inherits-nothing",
		dockerfile: `FROM ` + base + ` AS first
ENV FIRST=1
WORKDIR /first
USER nobody
FROM ` + base + `
RUN echo "$FIRST" > /second
`,
		runs: []run{{args: sh("cat /second; pwd; id -u")}},
	},
	{
		name: "target",
		dockerfile: `FROM ` + base + ` AS one
ENV STAGE=one
FROM ` + base + ` AS two
ENV STAGE=two
`,
		target: "one",
		runs:   []run{{args: sh("echo $STAGE")}},
	},
	{
		name: "from-scratch",
		dockerfile: `FROM scratch
COPY --from=busybox:1.36-musl /bin/busybox /bin/busybox
COPY hello.txt /hello.txt
WORKDIR /w
CMD ["/bin/busybox", "cat", "/hello.txt"]
`,
		files: map[string]file{"hello.txt": {data: "from scratch\n"}},
		runs:  []run{{args: nil}, {args: []string{"/bin/busybox", "sh", "-c", "pwd; echo $PATH"}}},
		note:  "docker gives images built from scratch a default PATH",
	},
	{
		name: "dockerignore",
		dockerfile: `FROM ` + base + `
COPY . /app
`,
		files: map[string]file{
			".dockerignore":     {data: "*.log\nsecret/\n!secret/keep.txt\n**/*.tmp\nDockerfile\n"},
			"keep.txt":          {data: "k"},
			"x.log":             {data: "l"},
			"secret/a":          {data: "s"},
			"secret/keep.txt":   {data: "s"},
			"nested/deep/b.tmp": {data: "t"},
			"nested/deep/c":     {data: "c"},
		},
		runs: []run{{args: listing("/app")}},
	},
	{
		name: "heredoc",
		dockerfile: `FROM ` + base + `
RUN <<EOT
mkdir /app
echo "heredoc run" > /app/run
echo "line two" >> /app/run
EOT
COPY <<EOT /app/file
heredoc copy
second
EOT
COPY <<-EOT /app/dedented
	indented
	EOT
RUN <<EOT1 <<EOT2
cat > /app/multi1
first
EOT1
cat > /app/multi2
second
EOT2
COPY --chmod=755 <<EOT /app/script
#!/bin/sh
echo hi
EOT
`,
		runs: []run{
			{args: listing("/app")},
			{args: sh("cat /app/run /app/file /app/dedented /app/multi1 /app/multi2 /app/script")},
		},
	},
	{
		name: "syntax",
		dockerfile: `# a comment
FROM ` + base + `

# comment between instructions
run mkdir /app \
    # a comment inside a continuation
    && echo "continued" > /app/cont
Run echo "mixed case" > /app/case
ENV  SPACED=1
RUN echo "$SPACED" > /app/spaced
`,
		runs: []run{{args: sh("cat /app/cont /app/case /app/spaced")}},
	},
	{
		name:       "escape-directive",
		dockerfile: "# escape=`\nFROM " + base + "\nRUN mkdir /app && echo back\\slash > /app/f `\n && echo cont >> /app/f\n",
		runs:       []run{{args: sh("cat /app/f")}},
	},
	{
		name: "copy-symlink-context",
		dockerfile: `FROM ` + base + `
COPY . /app
COPY link.txt /app/followed-or-not
`,
		files: map[string]file{
			"real.txt": {data: "real"},
			"link.txt": {symlink: "real.txt"},
			"dirlink":  {symlink: "sub"},
			"sub/x":    {data: "x"},
		},
		runs: []run{{args: listing("/app")}},
	},
	{
		name: "run-network-none",
		dockerfile: `FROM ` + base + `
RUN --network=none sh -c 'wget -T 5 -q -O /dev/null http://1.1.1.1/ 2>/dev/null; echo $?' > /isolated
RUN sh -c 'wget -T 5 -q -O /dev/null http://1.1.1.1/ 2>/dev/null; echo $?' > /default
`,
		runs: []run{{args: sh("cat /isolated /default")}},
		note: "RUN --network=none must have no network; RUN by default has it",
	},
	{
		name: "run-mount-cache",
		dockerfile: `FROM ` + base + `
RUN --mount=type=cache,target=/cache echo x > /cache/x && echo ok > /ok
`,
		runs: []run{{args: sh("cat /ok; ls /cache 2>&1")}},
		note: "RUN --mount is documented as unsupported",
	},
	{
		name: "copy-parents",
		dockerfile: `# syntax=docker/dockerfile:1.7-labs
FROM ` + base + `
COPY --parents a/b/*.txt /app/
`,
		files: map[string]file{"a/b/one.txt": {data: "1"}, "a/b/two.txt": {data: "2"}},
		runs:  []run{{args: listing("/app")}},
		note:  "COPY --parents is documented as unsupported",
	},
	{
		name: "copy-exclude",
		dockerfile: `# syntax=docker/dockerfile:1.7-labs
FROM ` + base + `
COPY --exclude=*.log . /app/
`,
		files: map[string]file{"a.txt": {data: "1"}, "b.log": {data: "2"}},
		runs:  []run{{args: listing("/app")}},
		note:  "COPY --exclude is documented as unsupported",
	},
	{
		name: "run-exit-status",
		dockerfile: `FROM ` + base + `
COPY notexec /notexec
`,
		files: map[string]file{"notexec": {data: "text", mode: 0o644}},
		runs: []run{
			{args: sh("exit 3")},
			{args: []string{"no-such-command"}},
			{args: []string{"/notexec"}},
			{args: sh("kill -9 $$")},
			{args: sh("echo out; echo err >&2; exit 1"), stderr: true},
		},
		note: "docker: 127 not found, 126 not executable, 137 killed",
	},
	{
		name:       "run-stdin",
		dockerfile: `FROM ` + base + "\n",
		runs: []run{
			{args: []string{"cat"}, stdin: "piped input\n"},
			{args: sh("wc -c"), stdin: "12345"},
		},
	},
	{
		name:       "run-process",
		dockerfile: `FROM ` + base + "\nRUN adduser -D app\n",
		runs: []run{
			{args: sh("id; pwd")},
			{args: sh("id -u; id -g; id -G"), user: "app"},
			{args: sh("id -u; id -g; id -G"), user: "app:root"},
			{args: sh("id -u; id -g"), user: "4321:4321"},
			{args: sh("cat /proc/1/cmdline | tr '\\0' ' '")},
			{args: sh("ls /dev | sort")},
		},
	},
	{
		name:       "run-filesystem-view",
		dockerfile: `FROM ` + base + "\n",
		runs: []run{
			{args: sh("ls -d /proc /sys /dev/pts /dev/shm /tmp; stat -c '%A %u:%g' / /tmp /root")},
			{args: sh("grep -c nameserver /etc/resolv.conf; grep -c localhost /etc/hosts")},
			{args: sh("mount | awk '{print $3, $5}' | sort")},
		},
		note: "runtime-provided files; differences are expected",
	},
	{
		name: "base-image-config",
		dockerfile: `FROM nginx:1.27-alpine
LABEL added=1
`,
		runs: []run{{args: sh("nginx -v 2>&1; echo $NGINX_VERSION; pwd")}},
		note: "inherits ENV, EXPOSE, ENTRYPOINT, CMD, STOPSIGNAL from the base",
	},
	{
		name:       "bad-dockerfile",
		dockerfile: "FROM " + base + "\nCOPY missing.txt /\n",
		buildFails: true,
	},
	{
		name:       "bad-instruction",
		dockerfile: "FROM " + base + "\nFROB x\n",
		buildFails: true,
	},
	{
		name:       "run-fails",
		dockerfile: "FROM " + base + "\nRUN exit 7\n",
		buildFails: true,
	},
	{
		name:       "copy-outside-context",
		dockerfile: "FROM " + base + "\nCOPY ../etc/passwd /\n",
		buildFails: true,
	},
}
