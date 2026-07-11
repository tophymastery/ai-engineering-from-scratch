# Canonical container build for the payment-sim fake (S-T7, 03 §5). Std-lib only, so the
# build stage needs nothing but the Go toolchain and the runtime is scratch.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /out/payment-sim .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/payment-sim /payment-sim
EXPOSE 8091
USER nonroot:nonroot
ENTRYPOINT ["/payment-sim"]
