export RESTIC_REPOSITORY=s3:https://s3.docs.test:9000/box1
read -r AWS_ACCESS_KEY_ID && export AWS_ACCESS_KEY_ID          # the box's key
read -rs AWS_SECRET_ACCESS_KEY && export AWS_SECRET_ACCESS_KEY
read -rs RESTIC_PASSWORD && export RESTIC_PASSWORD             # the repository's password
(sleep 1800; echo probe) | restic backup --stdin --stdin-filename rc-lock-probe
echo "[exit $?]"
restic list locks --no-lock | wc -l
