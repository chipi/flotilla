# Smokescreen (github.com/stripe/smokescreen, MIT), the egress proxy (ADR-0002, approved
# 2026-11-08), built from a pinned commit. Build context: the repo root (nothing of ours is copied).
FROM golang@sha256:cfa27959837144bf4649802ba38e59daffd0dd2a7994aa83daaf113285b0f551 AS build
ARG SMOKESCREEN_COMMIT=5b4b7aabca9bc70a8435358d648edd03294c299e
RUN git clone https://github.com/stripe/smokescreen.git /src \
 && cd /src && git checkout --detach "${SMOKESCREEN_COMMIT}" \
 && test "$(git rev-parse HEAD)" = "${SMOKESCREEN_COMMIT}" \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/smokescreen .

FROM gcr.io/distroless/static-debian12@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/smokescreen /usr/local/bin/smokescreen
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/smokescreen"]
