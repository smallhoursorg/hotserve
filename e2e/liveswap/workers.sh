#!/bin/sh
# The "workers" release's ./server: a shell leader that forks a worker
# (what npm/node trees look like to the supervisor), records both pids
# for the systemd suite, says hello to the journal, then becomes the
# app. Stopping or crashing this unit must take the worker with it.
sleep 300 &
echo "$$ $!" > pids.txt
echo "workers up on $PORT"
exec ./server-bin
