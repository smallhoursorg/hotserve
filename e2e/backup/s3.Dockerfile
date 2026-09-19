# The S3 server the backup suite's repository lives on: `rclone serve
# s3`, the static binary out of rclone's own image and nothing else.
# Verified with restic 0.18, multipart and `check --read-data` included.
# It has one key, which may do anything, and makes a bucket when asked —
# a stand-in for storage, not for a provider's policy, which the suite
# does not test. The Debian archive has no S3 server (its rclone, 1.60,
# has no `serve s3`), MinIO's community edition is archived, and
# SeaweedFS accepts a Deny policy without enforcing it.
#
# Pinned by digest; the tag is for Dependabot and for people.
FROM rclone/rclone:1.71@sha256:3103526c506266a9ecdf064efe99bf3677d92ef6407af124d8c56b4f49cbaa51 AS rclone

FROM scratch
COPY --from=rclone /usr/local/bin/rclone /rclone
# Data is a tmpfs mounted at /data (docker-compose.yml), which counts
# against the container's memory limit.
ENTRYPOINT ["/rclone", "serve", "s3", "--addr", ":9000", "/data"]
