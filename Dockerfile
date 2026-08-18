# Packages a statically linked binary that was built on the host.
#
# There is no builder stage and no base image, so `docker build` pulls nothing
# and works offline or behind a slow registry. fresh-cluster.sh produces the
# binary first, cross-compiling for the cluster's architecture:
#
#   CGO_ENABLED=0 GOOS=linux GOARCH=<arm64|amd64> go build -o dist/ufm-mock .
#
# The binary is static (CGO_ENABLED=0) and makes no outbound TLS calls, so it
# needs neither libc nor CA certificates.

# Use a minimal builder to ensure proper permissions
FROM busybox:latest AS builder
COPY dist/ufm-mock /ufm-mock
RUN chmod 755 /ufm-mock

FROM scratch
COPY --from=builder /ufm-mock /ufm-mock
EXPOSE 9888
# Numeric so Kubernetes can verify runAsNonRoot; scratch has no /etc/passwd.
USER 65532:65532
ENTRYPOINT ["/ufm-mock"]
