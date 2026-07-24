# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.24 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w \
      -X github.com/ankit373/mainspring/internal/build.Version=${VERSION} \
      -X github.com/ankit373/mainspring/internal/build.Commit=${COMMIT} \
      -X github.com/ankit373/mainspring/internal/build.Date=${DATE}" \
    -o /out/mainspring ./cmd/mainspring

# ---- runtime ----
# Distroless static: no shell, non-root, minimal attack surface. The binary is
# CGO-free so this works. NOTE: local engine backends (llama-server/mlx) are not
# in this image — provide one via a managed install onto a mounted volume, a
# derived image, or point Mainspring at a remote/adopted Ollama.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/mainspring /usr/local/bin/mainspring

# Config + models are expected on a mounted volume.
VOLUME ["/config", "/models"]
EXPOSE 11500

ENTRYPOINT ["/usr/local/bin/mainspring"]
CMD ["serve", "--addr", ":11500", "--config", "/config/config.yaml"]
