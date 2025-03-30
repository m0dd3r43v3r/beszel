#!/bin/bash

# Exit on error
set -e

# Version from command line or prompt
if [ -z "$1" ]; then
    read -p "Enter version number (e.g., 1.0.0): " VERSION
else
    VERSION=$1
fi

# Create release directory
RELEASE_DIR="release_${VERSION}"
mkdir -p "$RELEASE_DIR"

# Build for different platforms
PLATFORMS=(
    "linux/amd64"
    "linux/arm64"
    "linux/arm"
    "linux/mips64"
    "linux/riscv64"
    "darwin/amd64"
    "darwin/arm64"
    "freebsd/amd64"
    "freebsd/arm64"
    "windows/amd64"
    "windows/arm64"
)

echo "Building for different platforms..."
for PLATFORM in "${PLATFORMS[@]}"; do
    IFS='/' read -r OS ARCH <<< "$PLATFORM"
    echo "Building for $OS/$ARCH..."
    
    # Build the binary with the correct package path
    cd beszel
    if [ "$OS" = "windows" ]; then
        GOOS=$OS GOARCH=$ARCH go build -o "../$RELEASE_DIR/beszel-agent_${OS}_${ARCH}.exe" ./cmd/agent
    else
        GOOS=$OS GOARCH=$ARCH go build -o "../$RELEASE_DIR/beszel-agent_${OS}_${ARCH}" ./cmd/agent
    fi
    cd ..
done

# Create Debian packages for Linux builds
echo "Creating Debian packages..."
for BINARY in $RELEASE_DIR/beszel-agent_linux_*; do
    if [[ $BINARY == *".tar.gz" ]]; then
        continue
    fi
    ARCH=$(echo $BINARY | sed 's/.*linux_\(.*\)$/\1/')
    # Map Go arch to Debian arch
    case $ARCH in
        "amd64") DEB_ARCH="amd64" ;;
        "arm64") DEB_ARCH="arm64" ;;
        "arm") DEB_ARCH="armv6" ;;
        "mips64") DEB_ARCH="mips64_hardfloat" ;;
        "riscv64") DEB_ARCH="riscv64" ;;
    esac
    
    # Create temporary directory structure for .deb
    PKG_ROOT=$(mktemp -d)
    mkdir -p $PKG_ROOT/DEBIAN
    mkdir -p $PKG_ROOT/usr/local/bin
    
    # Copy binary
    cp $BINARY $PKG_ROOT/usr/local/bin/beszel-agent
    
    # Create control file
    cat > $PKG_ROOT/DEBIAN/control << EOF
Package: beszel-agent
Version: ${VERSION}
Architecture: ${DEB_ARCH}
Maintainer: m0dd3r43v3r
Description: Beszel Agent - System monitoring and management tool
EOF
    
    # Build .deb package
    dpkg-deb --build $PKG_ROOT "$RELEASE_DIR/beszel-agent_${VERSION}_linux_${DEB_ARCH}.deb"
    rm -rf $PKG_ROOT
done

# Create archives for each binary
echo "Creating archives..."
cd "$RELEASE_DIR"

# Create tar.gz archives for Unix-like systems
for BINARY in beszel-agent_*; do
    if [[ $BINARY != *.deb && $BINARY != *.exe && $BINARY != *checksums.txt ]]; then
        OS_ARCH=$(echo $BINARY | sed 's/beszel-agent_\(.*\)$/\1/')
        tar -czf "beszel_${OS_ARCH}.tar.gz" "$BINARY"
        rm "$BINARY"
    fi
done

# Create zip archives for Windows binaries
for BINARY in *.exe; do
    if [ -f "$BINARY" ]; then
        OS_ARCH=$(echo $BINARY | sed 's/beszel-agent_\(.*\).exe$/\1/')
        zip "beszel-agent_${OS_ARCH}.zip" "$BINARY"
        rm "$BINARY"
    fi
done

# Create checksums file
echo "Creating checksums file..."
sha256sum * > "beszel_${VERSION}_checksums.txt"

cd ..

echo "Release files prepared in $RELEASE_DIR:"
ls -lh "$RELEASE_DIR"

echo "
To create a GitHub release:

1. Go to your GitHub repository
2. Click on 'Releases' in the right sidebar
3. Click 'Create a new release'
4. Choose tag 'v${VERSION}'
5. Add a title and description
6. Upload all files from $RELEASE_DIR
" 