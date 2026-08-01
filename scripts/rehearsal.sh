#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
compose_file="$repo_root/deploy/rehearsal/compose.yaml"
backup_dir="$repo_root/deploy/rehearsal/backups"
mkdir -p "$backup_dir"

compose() {
  docker compose -f "$compose_file" "$@"
}

wait_for_health() {
  attempts=0
  until curl --fail --silent "http://127.0.0.1:${REHEARSAL_PORT:-18000}/health" >/dev/null; do
    attempts=$((attempts + 1))
    if [ "$attempts" -ge 30 ]; then
      compose logs web
      return 1
    fi
    sleep 1
  done
}

case "${1:-}" in
  start)
    compose up -d --build web
    wait_for_health
    ;;
  stop)
    compose down
    ;;
  reset)
    compose down --volumes
    ;;
  seed)
    compose run --rm tools seed --summaries "${2:-1000}"
    ;;
  backup)
    name="${2:-rehearsal-backup-$(date -u +%Y%m%dT%H%M%SZ).tar.gz}"
    compose stop web
    compose run --rm tools backup "/backups/$name"
    compose start web
    printf '%s\n' "$backup_dir/$name"
    ;;
  restore)
    if [ "$#" -ne 2 ]; then
      echo "usage: $0 restore <backup-file>" >&2
      exit 2
    fi
    name=$(basename -- "$2")
    compose stop web
    compose run --rm tools restore "/backups/$name"
    compose start web
    wait_for_health
    ;;
  inspect-migration)
    compose stop web
    compose run --rm tools inspect
    TRONBYT_PUSH_CLEANUP_DRY_RUN=true compose up -d web
    wait_for_health
    compose logs --since 2m web
    ;;
  poll)
    shift
    compose run --rm tools poll "$@"
    ;;
  load)
    shift
    compose run --rm tools load "$@"
    ;;
  *)
    echo "usage: $0 {start|stop|reset|seed [count]|backup [name]|restore <file>|inspect-migration|poll [flags]|load [flags]}" >&2
    exit 2
    ;;
esac
