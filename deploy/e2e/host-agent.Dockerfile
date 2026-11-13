# The e2e's stand-in host: flotilla-host on a small Linux image, with /bin/true for a harmless action.
# Not shipped: the real host agent is built by the operator and installed over SSH (ADR-0006 §9).
# Build context: the repo root.
FROM golang@sha256:cfa27959837144bf4649802ba38e59daffd0dd2a7994aa83daaf113285b0f551 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/flotilla-host ./cmd/flotilla-host

FROM python@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b
COPY --from=build /out/flotilla-host /usr/local/bin/flotilla-host
RUN useradd --system --uid 10100 --create-home flotilla-host \
 && mkdir -p /var/db/flotilla-host /var/log/flotilla-host \
 && chown flotilla-host /var/db/flotilla-host /var/log/flotilla-host
USER flotilla-host
ENTRYPOINT ["/usr/local/bin/flotilla-host"]
