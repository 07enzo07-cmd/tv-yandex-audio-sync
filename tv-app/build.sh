#!/bin/bash
set -e

DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$DIR"

# Detect Java
if [ -z "$JAVA_HOME" ]; then
    if [ -d "/opt/homebrew/opt/openjdk@17" ]; then
        export JAVA_HOME="/opt/homebrew/opt/openjdk@17"
    elif [ -d "/opt/homebrew/opt/openjdk" ]; then
        export JAVA_HOME="/opt/homebrew/opt/openjdk"
    elif [ -x "/usr/libexec/java_home" ]; then
        export JAVA_HOME=$(/usr/libexec/java_home 2>/dev/null || true)
    fi
fi
if [ -n "$JAVA_HOME" ]; then
    export PATH="$JAVA_HOME/bin:$PATH"
fi
export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"

# Check if full Android SDK with build-tools and platforms is available
SDK_DIR=""
if [ -n "$ANDROID_HOME" ] && [ -d "$ANDROID_HOME/build-tools" ] && [ -d "$ANDROID_HOME/platforms" ]; then
    SDK_DIR="$ANDROID_HOME"
elif [ -d "/Users/leo/Library/Android/sdk/build-tools" ] && [ -d "/Users/leo/Library/Android/sdk/platforms" ]; then
    SDK_DIR="/Users/leo/Library/Android/sdk"
elif [ -d "$HOME/Library/Android/sdk/build-tools" ] && [ -d "$HOME/Library/Android/sdk/platforms" ]; then
    SDK_DIR="$HOME/Library/Android/sdk"
fi

rm -rf bin
mkdir -p bin/classes

if [ -n "$SDK_DIR" ]; then
    BUILD_TOOLS_DIR="$SDK_DIR/build-tools"
    BUILD_TOOLS_VERSION=$(ls -1 "$BUILD_TOOLS_DIR" | sort -V | tail -n 1)
    BUILD_TOOLS="$BUILD_TOOLS_DIR/$BUILD_TOOLS_VERSION"

    PLATFORMS_DIR="$SDK_DIR/platforms"
    PLATFORM_VERSION=$(ls -1 "$PLATFORMS_DIR" | sort -V | tail -n 1)
    PLATFORM="$PLATFORMS_DIR/$PLATFORM_VERSION/android.jar"

    echo "Using Android SDK: $SDK_DIR"
    echo "=== Step 1: Generating R.java with aapt ==="
    "$BUILD_TOOLS/aapt" package -f -m \
        -J src \
        -M AndroidManifest.xml \
        -S res \
        -I "$PLATFORM"

    echo "=== Step 2: Compiling Java sources ==="
    javac -source 1.8 -target 1.8 -cp "$PLATFORM" -d bin/classes \
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
    "$BUILD_TOOLS/apksigner" sign --ks debug.keystore \
        --ks-key-alias androiddebugkey \
        --ks-pass pass:android \
        --key-pass pass:android \
        --v1-signing-enabled true \
        --v2-signing-enabled true \
        --out TvAudioSync.apk \
        bin/aligned.apk
else
    echo "Using standalone build mode (android-stubs.jar + d8 + base-resources.apk)"
    STUBS_JAR="$DIR/android-stubs.jar"
    BASE_APK="$DIR/base-resources.apk"

    echo "=== Step 1: Compiling Java sources against android-stubs.jar ==="
    javac -source 1.8 -target 1.8 -cp "$STUBS_JAR" -d bin/classes \
        src/dev/antigravity/audiosync/*.java

    echo "=== Step 2: Compiling classes to DEX with d8 ==="
    d8 --lib "$STUBS_JAR" --output bin/ \
        bin/classes/dev/antigravity/audiosync/*.class

    echo "=== Step 3: Packaging DEX into base-resources.apk ==="
    cp "$BASE_APK" bin/unsigned.apk
    zip -d bin/unsigned.apk "META-INF/*" "classes/*" 2>/dev/null || true
    (cd bin && zip -u unsigned.apk classes.dex)

    echo "=== Step 4: Zipaligning and Signing APK (v1 + APK Signature Scheme v2) ==="
    python3 "$DIR/sign_v2.py" bin/unsigned.apk TvAudioSync.apk debug.keystore
fi

echo "=== Build Success: TvAudioSync.apk ==="
ls -lh TvAudioSync.apk
