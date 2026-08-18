#!/usr/bin/env bash

VERSION=$1

if [[ -z $VERSION ]]; then
    echo "Usage: $0 <version>"
    exit 1
fi

for i in $(ls release); do
    fileName="agent-utils-${i}-${VERSION}.tar.gz"

    tar -czvf "./release/${fileName}" -C "./release/${i}/" agent-utils
done
