#!/bin/bash
set -euxo pipefail

echo go build -o /workspace/arduino-editor/arduino-ide-extension/build/arduino-language-server > /workspace/arduino-language-server/build.sh
chmod +x /workspace/arduino-language-server/build.sh

cd /workspace
git clone https://github.com/bcmi-labs/arduino-editor
cd arduino-editor
yarn

echo "starting an Arduino IDE with: yarn --cwd /workspace/arduino-editor/browser-app start"
yarn --cwd /workspace/arduino-editor/browser-app start
