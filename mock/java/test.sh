#!/usr/bin/env bash
# Build the Java mock, then compile and run its self-tests (JDK only).
set -euo pipefail
cd "$(dirname "$0")"
./build.sh >/dev/null
rm -rf build/test && mkdir -p build/test
javac -Xlint:all -Werror --release 21 -cp build/classes -d build/test test/lmbp/*.java
exec java -Dlmbp.root=../.. -cp build/classes:build/test lmbp.SelfTest
