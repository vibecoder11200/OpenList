#!/bin/sh

umask ${UMASK}

if [ "$1" = "version" ]; then
  ./openlist version
else
  # Check file of /opt/openlist/data permissions for current user
  # 检查当前用户是否有当前目录的写和执行权限
  if [ -d ./data ]; then
    if ! [ -w ./data ] || ! [ -x ./data ]; then
  cat <<EOF
Error: Current user does not have write and/or execute permissions for the ./data directory: $(pwd)/data
Please visit https://doc.oplist.org/guide/installation/docker#for-version-after-v4-1-0 for more information.
错误：当前用户没有 ./data 目录（$(pwd)/data）的写和/或执行权限。
请访问 https://doc.oplist.org/guide/installation/docker#v4-1-0-%E4%BB%A5%E5%90%8E%E7%89%88%E6%9C%AC 获取更多信息。
Exiting...
EOF
      exit 1
    fi
  fi

  # Enable the bundled offline-download services (aria2, qBittorrent,
  # Transmission) as runit services. Each service seeds its persistent
  # config under /opt/openlist/data/service/<name> on first start.
  enable_service() {
    name="$1"
    want="$2"
    if [ "$want" = "true" ]; then
      mkdir -p "/opt/service/start/$name"
      cp -r "/opt/mod/service/$name/." "/opt/service/start/$name/"
    else
      rm -rf "/opt/service/start/$name"
    fi
  }
  enable_service aria2 "${RUN_ARIA2:-true}"
  enable_service qbittorrent "${RUN_QBIT:-true}"
  enable_service transmission "${RUN_TRANS:-true}"

  if [ -n "$(ls /opt/service/start 2>/dev/null)" ]; then
    runsvdir /opt/service/start &
  fi

  exec ./openlist server --no-prefix
fi
