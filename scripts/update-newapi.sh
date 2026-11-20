#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

repo_dir="${1:-/root/dev/newapi}"
for required in git docker tar flock; do
  command -v "$required" >/dev/null || {
    echo "缺少命令：$required" >&2
    exit 1
  }
done

cd "$repo_dir"
repo_dir="$(pwd -P)"
[[ -d .git && -f docker-compose.yml ]] || {
  echo "请在已有 Git 仓库且含 docker-compose.yml 的部署目录运行。" >&2
  exit 1
}

exec 9>.git/new-api-update.lock
flock -n 9 || {
  echo "已有更新任务正在运行。" >&2
  exit 1
}

docker compose version >/dev/null
docker info >/dev/null

build_dir=""
trap 'if [[ -n "$build_dir" ]]; then rm -rf -- "$build_dir"; fi' EXIT
trap 'echo "更新失败，请检查上方错误信息。备份目录：${backup_dir:-尚未创建}" >&2' ERR

echo "正在拉取 origin/main..."
git fetch --depth=1 origin refs/heads/main
target_commit="$(git rev-parse FETCH_HEAD)"
backup_dir="$(mktemp -d "$repo_dir/../newapi-config-backup.XXXXXX")"
cp -p docker-compose.yml "$backup_dir/docker-compose.yml"

# Build from the exact fetched tree so runtime data and local artifacts stay out of the image.
build_dir="$(mktemp -d /tmp/new-api-build.XXXXXX)"
mkdir "$build_dir/source"
git archive "$target_commit" | tar -xf - -C "$build_dir/source"
cat > "$build_dir/compose.build.yml" <<YAML
services:
  new-api:
    image: new-api:local
    build:
      context: "$build_dir/source"
      dockerfile: Dockerfile
YAML

compose=(docker compose -f "$repo_dir/docker-compose.yml" -f "$build_dir/compose.build.yml")
"${compose[@]}" config --quiet

echo "正在更新源码，保留 data、logs、docker-compose.yml 和 .env..."
git reset --mixed --quiet "$target_commit"
git restore --source="$target_commit" --worktree -- . \
  ':(top,exclude)data' \
  ':(top,exclude)logs' \
  ':(top,exclude)docker-compose.yml' \
  ':(top,exclude).env'

echo "正在构建 new-api..."
"${compose[@]}" build new-api

echo "正在重建 new-api 容器并等待就绪..."
"${compose[@]}" up -d --no-deps --force-recreate --pull never \
  --wait --wait-timeout 120 new-api
"${compose[@]}" ps new-api

echo "部署完成，提交：$target_commit"
echo "docker-compose.yml 备份：$backup_dir/docker-compose.yml"
