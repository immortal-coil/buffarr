FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
# Build stamp shown on the About page. deploy.sh passes VERSION from git; a
# plain `docker build` with no --build-arg leaves it "dev".
ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
      -ldflags "-X 'github.com/immortal-coil/buffarr/internal/web.BuildVersion=${VERSION}'" \
      -o /out/backend ./cmd/backend

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/backend /usr/local/bin/backend
ENTRYPOINT ["/usr/local/bin/backend"]
