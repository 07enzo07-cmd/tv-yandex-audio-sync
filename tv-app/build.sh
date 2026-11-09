#!/bin/bash
set -e

DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$DIR"

# Detect Android SDK
if [ -n "$ANDROID_HOME" ]; then
    SDK_DIR="$ANDROID_HOME"
elif [ -d "$HOME/Library/Android/sdk" ]; then
    SDK_DIR="$HOME/Library/Android/sdk"
elif [ -d "$HOME/Android/Sdk" ]; then
    SDK_DIR="$HOME/Android/Sdk"
else
    echo "Error: ANDROID_HOME is not set and SDK not found in default paths."
    exit 1
fi

# Detect Build Tools
BUILD_TOOLS_DIR="$SDK_DIR/build-tools"
BUILD_TOOLS_VERSION=$(ls -1 "$BUILD_TOOLS_DIR" | sort -V | tail -n 1)
BUILD_TOOLS="$BUILD_TOOLS_DIR/$BUILD_TOOLS_VERSION"

# Detect Platform android.jar
PLATFORMS_DIR="$SDK_DIR/platforms"
PLATFORM_VERSION=$(ls -1 "$PLATFORMS_DIR" | sort -V | tail -n 1)
PLATFORM="$PLATFORMS_DIR/$PLATFORM_VERSION/android.jar"

echo "Using Android SDK: $SDK_DIR"
echo "Using Build Tools: $BUILD_TOOLS"
echo "Using Platform Jar: $PLATFORM"

# Detect Java
if [ -z "$JAVA_HOME" ]; then
    if [ -d "/opt/homebrew/opt/openjdk@17" ]; then
        export JAVA_HOME="/opt/homebrew/opt/openjdk@17"
    elif [ -d "/usr/libexec/java_home" ]; then
        export JAVA_HOME=$(/usr/libexec/java_home)
    fi
fi
if [ -n "$JAVA_HOME" ]; then
    export PATH="$JAVA_HOME/bin:$PATH"
fi

export PATH="$BUILD_TOOLS:$PATH"

rm -rf bin
mkdir -p bin/classes

echo "=== Step 1: Generating R.java with aapt ==="
"$BUILD_TOOLS/aapt" package -f -m \
    -J src \
    -M AndroidManifest.xml \
    -S res \
    -I "$PLATFORM"

echo "=== Step 2: Compiling Java sources ==="
javac -cp "$PLATFORM" -d bin/classes \
    src/dev/antigravity/audiosync/*.java

echo "=== Step 3: Compiling classes to DEX with d8 ==="
"$BUILD_TOOLS/d8" --lib "$PLATFORM" --output bin/ \
    bin/classes/dev/antigravity/audiosync/*.class

echo "=== Step 4: Packaging unsigned APK with aapt ==="
"$BUILD_TOOLS/aapt" package -f \
    -M AndroidManifest.xml \
    -S res \
    -I "$PLATFORM" \
    -F bin/unaligned.apk \
    bin/

echo "=== Step 5: Aligning APK with zipalign ==="
"$BUILD_TOOLS/zipalign" -f -p 4 bin/unaligned.apk bin/aligned.apk

echo "=== Step 6: Signing APK with apksigner ==="
KEYSTORE="debug.keystore"
if [ ! -f "$KEYSTORE" ]; then
    echo "Generating debug.keystore..."
    keytool -genkey -v -keystore "$KEYSTORE" -alias androiddebugkey -storepass android -keypass android -keyalg RSA -keysize 2048 -validity 10000 -dname "CN=Android Debug,O=Android,C=US"
fi

"$BUILD_TOOLS/apksigner" sign --ks "$KEYSTORE" \
    --ks-key-alias androiddebugkey \
    --ks-pass pass:android \
    --key-pass pass:android \
    --out TvAudioSync.apk \
    bin/aligned.apk

echo "=== Build Success: TvAudioSync.apk ==="
