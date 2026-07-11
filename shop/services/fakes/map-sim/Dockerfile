# Canonical container build for the map-sim fake (S-T7, 03 §5). Std-lib only, so the
# build stage needs nothing but the Go toolchain and the runtime is scratch.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /out/map-sim .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/map-sim /map-sim
EXPOSE 8092
USER nonroot:nonroot
ENTRYPOINT ["/map-sim"]
