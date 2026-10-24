# Build a static binary in one stage, ship it in a small image.
FROM alpine:3.20 AS build
RUN apk add --no-cache gcc musl-dev
COPY hello.c /src/
RUN gcc -static -O2 -o /hello /src/hello.c

FROM alpine:3.20
RUN adduser -D app
COPY --from=build --chown=app /hello /usr/local/bin/hello
USER app
CMD ["hello"]
