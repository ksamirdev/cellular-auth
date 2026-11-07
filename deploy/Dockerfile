FROM mirror.gcr.io/library/golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

# The chaos runner needs the docker CLI to kill and revive cells.
FROM mirror.gcr.io/library/docker:29-cli AS chaos
COPY --from=build /out/chaos /usr/local/bin/chaos
ENTRYPOINT ["chaos"]

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/cell /out/router /out/replicator /usr/local/bin/
