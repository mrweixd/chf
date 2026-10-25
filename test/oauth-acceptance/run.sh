#!/usr/bin/env bash
set -euo pipefail
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
run_dir=$(mktemp -d)
trap 'rm -rf "$run_dir"' EXIT
mkdir -p "$script_dir/results" "$run_dir/bin"
clone_commit() {
 local name=$1 repo=$2 sha=$3
 git init -q "$run_dir/$name"
 git -C "$run_dir/$name" remote add origin "https://github.com/$repo.git"
 git -C "$run_dir/$name" fetch --depth=1 origin "$sha"
 git -C "$run_dir/$name" checkout -q --detach FETCH_HEAD
 test "$(git -C "$run_dir/$name" rev-parse HEAD)" = "$sha"
}
clone_commit chf mrweixd/chf ff4c923df6fd97f8557a177bbf69d59bd92e98b6
clone_commit pcf mrweixd/pcf 31fbcbdef5fcb6f458a8a82611edf17399557035
clone_commit nef mrweixd/nef 127cdc39c2f824d514892d3a4d6f011e5598d5dd
clone_commit nrf Zach1113/nrf 68e575dc5584040484ef9e58f87b6a23bd392d99
for nf in chf pcf nef nrf; do
 cp "$script_dir/sources/${nf}_live_test.go" "$run_dir/$nf/internal/sbi/oauth_live_acceptance_test.go"
done
(
 cd "$run_dir/nrf"
 go mod edit -replace=github.com/free5gc/openapi=github.com/mrweixd/openapi@v0.0.0-20261025094827-bd904d23aed6
)
for nf in chf pcf nef; do
 (
  cd "$run_dir/$nf"
  # The test-only MongoDB client adds dependencies in this disposable checkout.
  go test -mod=mod -race -c ./internal/sbi -o "$run_dir/bin/$nf.test"
 )
done
if [[ -z "${OAUTH_MONGOD:-}" ]]; then
 curl --fail --location --retry 3 https://fastdl.mongodb.org/linux/mongodb-linux-x86_64-ubuntu2404-8.0.16.tgz -o "$run_dir/mongo.tgz"
 tar --no-same-owner -xzf "$run_dir/mongo.tgz" -C "$run_dir"
 export OAUTH_MONGOD="$run_dir/mongodb-linux-x86_64-ubuntu2404-8.0.16/bin/mongod"
fi
export OAUTH_BIN_DIR="$run_dir/bin"
{
 go version
 "$OAUTH_MONGOD" --version
} > "$script_dir/results/versions.log"
(
 cd "$run_dir/nrf"
 go test -mod=mod -race -count=1 -v ./internal/sbi -run '^TestLiveOAuthAcceptance$' -timeout=5m
) 2>&1 | tee "$script_dir/results/acceptance.log"
