FROM --platform=$BUILDPLATFORM golang:1.25 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/manager ./cmd

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source=https://github.com/shanurwan/cnpg-restore-check
LABEL org.opencontainers.image.description="Scheduled restore checks for CloudNativePG backups"
LABEL org.opencontainers.image.licenses=Apache-2.0
COPY --from=build /out/manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
