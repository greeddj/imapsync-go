# syntax=docker/dockerfile:1
FROM gcr.io/distroless/static-debian13:nonroot
ARG TARGETPLATFORM
WORKDIR /
COPY ${TARGETPLATFORM}/dist/imapsync-go /imapsync-go
ENTRYPOINT [ "/imapsync-go" ]
