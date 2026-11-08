# flotillad + the flotilla CLI, static, on distroless (ADR-0003). Build context: the repo root.
#   docker build -f deploy/docker/flotillad.Dockerfile --build-arg VERSION=sha-<7> -t flotilla-flotillad:sha-<7> .
FROM golang@sha256:cfa27959837144bf4649802ba38e59daffd0dd2a7994aa83daaf113285b0f551 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
ARG VERSION=dev
# timetzdata: the store's local day (the budget day) does not depend on the image's zoneinfo
RUN CGO_ENABLED=0 go build -trimpath -tags timetzdata -ldflags "-s -w -X main.version=${VERSION}" -o /out/flotillad ./cmd/flotillad \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/flotilla ./cmd/flotilla

FROM gcr.io/distroless/static-debian12@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/flotillad /out/flotilla /usr/local/bin/
# the specs travel with the image: a spec change is a deploy (ADR-0002)
COPY fleets /opt/flotilla/fleets
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/flotillad"]
CMD ["-config", "/etc/flotilla/flotillad.yaml"]
