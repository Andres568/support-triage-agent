# One image, the commands that run in it: /app/commerce-api, /app/worker and
# /app/migrate. Compose services and Cloud Run pick theirs by entrypoint, so
# one digest is built, scanned and deployed. cmd/eval is not shipped: it
# needs evals/ (cases, cassettes), which stays out of the image and runs
# from the devShell (make eval).
# Base images are pinned by digest (ADR-0001: trust hashes, not names).
# The Go version must match flake.nix (go_1_27 → 1.27.1).

FROM golang:1.27.1-trixie@sha256:433790e515d27dc6003e847e644cc0af956985cf315c1c58a3b73ee2dd305183 AS build
WORKDIR /src
ENV GOTOOLCHAIN=local CGO_ENABLED=0

# Dependencies first: this layer is cached until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/commerce-api ./cmd/worker ./cmd/migrate

# Distroless: no shell, no package manager.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=build /out/ /app/
# The :nonroot user, numerically: runtimes that check runAsNonRoot can
# verify a uid, not a name.
USER 65532:65532
