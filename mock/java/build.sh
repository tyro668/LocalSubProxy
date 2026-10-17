#!/usr/bin/env bash
# Build the Java mock gateway into build/lmbp-mock.jar with only the JDK (21+; javac + jar).
set -euo pipefail
cd "$(dirname "$0")"
rm -rf build/classes && mkdir -p build/classes/schemas
cp ../../spec/schemas/defs.json ../../spec/schemas/task.run.payload.json ../../spec/schemas/hello.json ../../spec/schemas/models.update.json build/classes/schemas/
javac -Xlint:all -Xlint:-serial -Werror --release 21 -d build/classes src/lmbp/*.java
jar --create --file build/lmbp-mock.jar --main-class lmbp.Main -C build/classes .
echo "built mock/java/build/lmbp-mock.jar"
