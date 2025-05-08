# ---------- build stage ----------
    FROM golang:1.23 AS build
    WORKDIR /src
    
    # leverage Docker cache
    COPY go.* ./
    RUN go mod download
    
    # copy the entire source tree
    COPY . .
    
    # build static binaries
    RUN CGO_ENABLED=0 go build -o /out/server ./cmd/server && \
        CGO_ENABLED=0 go build -o /out/client ./cmd/client
    
    # ---------- runtime stage ----------
FROM gcr.io/distroless/static
LABEL org.opencontainers.image.source="https://github.com/<your-repo>/distributed_object_store"

COPY --from=build /out/server /bin/server
COPY --from=build /out/client /bin/client

# default: look for /etc/avid/config.yaml if no args are given
ENTRYPOINT ["/bin/server"]
CMD ["-config","/etc/avid/config.yaml"]
