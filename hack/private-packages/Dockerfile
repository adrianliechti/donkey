# E2E ONLY — authenticated TLS pass-through package mirror.
FROM gcr.io/distroless/static-debian12:nonroot
COPY private-packages /private-packages
ENTRYPOINT ["/private-packages"]
