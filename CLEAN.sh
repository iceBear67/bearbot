#!/bin/sh
# Restores the owners of the bearbot volumes (DESIGN 4.3, 4.4) inside the running container, then restarts it.
set -eu

docker exec bearbot sh -euc '
chown -hR bearbot:bearbot /bearbot/profile
chown root:root /bearbot/profile
chown -hR root:root /bearbot/profile/bearbot /bearbot/config
chown -hR bearbot-gw:bearbot-gw /bearbot/profile/bearbot/gateway
chown -hR bearbot:bearbot /bearbot/profile/bearbot/recovery-sessions /bearbot/workspace
[ ! -d /bearbot/workspace/inbox ] || chown -hR bearbot-gw:bearbot /bearbot/workspace/inbox
chmod -R g+rwX /bearbot/workspace
find /bearbot/workspace -type d -exec chmod g+s {} +
'
docker restart bearbot
