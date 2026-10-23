# Dockerfile de produccion para ARGOS backend.
# Multi-stage: compila un binario estatico y lo copia a una imagen distroless
# (sin shell ni gestor de paquetes, usuario sin privilegios).
#
#   docker run <imagen>                 -> argos serve
#   docker run <imagen> migrate up      -> migraciones (mismo binario)

FROM golang:1.27-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/argos ./cmd/argos


FROM gcr.io/distroless/static-debian13:nonroot AS runtime

COPY --from=builder /out/argos /usr/local/bin/argos

USER nonroot:nonroot

EXPOSE 8000

ENTRYPOINT ["argos"]
CMD ["serve"]
