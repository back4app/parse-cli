#!/bin/bash

TMP_FILE=/tmp/back4app.tmp
if [ -e ${TMP_FILE} ]; then
  echo "Cleaning up from previous install failure"
  rm -rf ${TMP_FILE}
fi
echo "Fetching latest version ..."

latest=3.0.6-beta-5
# latest=$(curl https://parsecli.back4app.com/supported?version=latest)

url="https://github.com/back4app/parse-cli/releases/download/release_${latest}/b4a"

if [ `uname` -eq "Linux" ]; then
  url="${url}_linux"
fi

curl --progress-bar --compressed -Lo ${TMP_FILE} ${url}

if [ ! -d /usr/local/bin ]; then
  echo "Making /usr/local/bin"
  mkdir -p /usr/local/bin
fi

echo "Installing ..."
mv /tmp/back4app.tmp /usr/local/bin/b4a
chmod 755 /usr/local/bin/b4a
