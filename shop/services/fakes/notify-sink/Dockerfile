# Canonical container build for the notify-sink fake (S-T7, 03 §5). Std-lib only, so the
# build stage needs nothing but the Go toolchain and the runtime is scratch.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /out/notify-sink .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/notify-sink /notify-sink
EXPOSE 8093
USER nonroot:nonroot
ENTRYPOINT ["/notify-sink"]
