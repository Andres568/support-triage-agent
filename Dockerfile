# One image, the commands that run in it: /app/commerce-api, /app/worker and
# /app/migrate. Compose services and Cloud Run pick theirs by entrypoint, so
# one digest is built, scanned and deployed. cmd/eval is not shipped: it
# needs evals/ (cases, cassettes), which stays out of the image and runs
# from the devShell (make eval).
# Base images are pinned by digest (ADR-0001: trust hashes, not names).
# The Go version must match flake.nix (go_1_27 → 1.27.1).

FROM golang:1.27.1-trixie@sha256:3b77fc618ec235a1ab412de7737f120dd507c57e8d87de4cbb7994fb94275ed5 AS build
WORKDIR /src
ENV GOTOOLCHAIN=local CGO_ENABLED=0

# Dependencies first: this layer is cached until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/commerce-api ./cmd/worker ./cmd/migrate

# Distroless: no shell, no package manager.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
COPY --from=build /out/ /app/
# The :nonroot user, numerically: runtimes that check runAsNonRoot can
# verify a uid, not a name.
USER 65532:65532
