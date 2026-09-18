# An S3 endpoint for the backup suite: rclone's `serve s3`, and nothing
# else. It stands in for B2 or S3 so that the e2e exercises restic's own
# s3: backend through the real job sandbox — credentials arriving in the
# settings file, the upload, the read-back, a restore.
#
# The binary comes from rclone's own published image, pinned by digest
# (Dependabot keeps it current), and is the ONLY thing in the final
# image: rclone is a static Go binary, so the Alpine layer it ships on —
# a shell, a package manager and some twenty packages — is left behind
# in the build stage. No OS, no shell, nothing else to be vulnerable.
#
# What it is not asked to prove: that a storage provider enforces its
# access policies. That is the provider's job; see docs/backups.md.
FROM rclone/rclone:1.75.1@sha256:45401ad7410db1d67ffdb58e19059ad20b0d8e0285a60e38bbec55cc1019c7a5 AS rclone

FROM scratch
COPY --from=rclone /usr/local/bin/rclone /rclone
# nobody: nothing here needs an identity, let alone root.
USER 65534:65534
ENTRYPOINT ["/rclone"]
