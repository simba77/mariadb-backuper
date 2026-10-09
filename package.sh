#!/usr/bin/env bash

# Builds the deb packages of the binaries build.sh made: bash package.sh 1.0.0
# nfpm is run at a pinned version, nothing has to be installed beforehand.
set -e

version=${1:?usage: package.sh <version>}
architectures=("amd64" "arm64")

# nfpm takes the files from fixed paths, the binary of every architecture is
# put in place before its package
mkdir -p build/package
for arch in "${architectures[@]}"
do
	cp "build/linux-$arch" build/package/mariadb-backuper
	env VERSION=$version ARCH=$arch go run github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.47.0 \
		package --config nfpm.yaml --packager deb --target build/
done
# Only the packages and the binaries go to the release
rm -r build/package
