#!/usr/bin/env bash

# The backuper runs on the MariaDB server, so only Linux binaries are built.
# CGO is off to get static binaries that do not depend on the libc of the server.
platforms=("linux/amd64" "linux/arm64")

for platform in "${platforms[@]}"
do
	platform_split=(${platform//\// })
	GOOS=${platform_split[0]}
	GOARCH=${platform_split[1]}
	output_name=$GOOS'-'$GOARCH

	env CGO_ENABLED=0 GOOS=$GOOS GOARCH=$GOARCH go build -o build/$output_name
	if [ $? -ne 0 ]; then
   		echo 'An error has occurred! Aborting the script execution...'
		exit 1
	fi
done
