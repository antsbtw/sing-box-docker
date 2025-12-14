#!/bin/bash
#
# 编译 sing-box 二进制文件（包含 v2ray_api 和 utls 支持）
# 运行此脚本后，将生成的二进制文件上传到 GitHub Release
#

set -e

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

# sing-box 版本
SINGBOX_VERSION="${SINGBOX_VERSION:-1.10.7}"

echo -e "${GREEN}========================================${NC}"
echo -e "${GREEN}  sing-box Binary Builder${NC}"
echo -e "${GREEN}  Version: v${SINGBOX_VERSION}${NC}"
echo -e "${GREEN}  Tags: with_v2ray_api,with_utls,with_reality_server${NC}"
echo -e "${GREEN}========================================${NC}"

# 检查 Go 版本
if ! command -v go &> /dev/null; then
    echo -e "${RED}Go is not installed. Please install Go 1.23+${NC}"
    exit 1
fi

GO_VERSION=$(go version | sed -E 's/.*go([0-9]+\.[0-9]+).*/\1/')
echo -e "${YELLOW}Go version: ${GO_VERSION}${NC}"

# 创建输出目录
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
OUTPUT_DIR="${SCRIPT_DIR}/../dist"
mkdir -p "$OUTPUT_DIR"
OUTPUT_DIR="$(cd "$OUTPUT_DIR" && pwd)"

# 下载 sing-box 源码
echo -e "${GREEN}Downloading sing-box v${SINGBOX_VERSION} source...${NC}"
TEMP_DIR=$(mktemp -d)
cd "$TEMP_DIR"
git clone --depth 1 --branch "v${SINGBOX_VERSION}" https://github.com/SagerNet/sing-box.git
cd sing-box

# 编译不同架构
ARCHS=("amd64" "arm64")

for ARCH in "${ARCHS[@]}"; do
    echo -e "${GREEN}Building for linux/${ARCH}...${NC}"

    OUTPUT_FILE="${OUTPUT_DIR}/sing-box-linux-${ARCH}"

    CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH \
        go build -tags "with_v2ray_api,with_utls,with_reality_server" \
        -ldflags "-s -w" \
        -o "$OUTPUT_FILE" \
        ./cmd/sing-box

    if [ -f "$OUTPUT_FILE" ]; then
        chmod +x "$OUTPUT_FILE"
        SIZE=$(du -h "$OUTPUT_FILE" | cut -f1)
        echo -e "${GREEN}✓ Built: sing-box-linux-${ARCH} (${SIZE})${NC}"
    else
        echo -e "${RED}✗ Failed to build for ${ARCH}${NC}"
        exit 1
    fi
done

# 清理
cd /
rm -rf "$TEMP_DIR"

echo ""
echo -e "${GREEN}========================================${NC}"
echo -e "${GREEN}  Build Complete!${NC}"
echo -e "${GREEN}========================================${NC}"
echo ""
echo -e "Output files in: ${YELLOW}${OUTPUT_DIR}${NC}"
ls -la "$OUTPUT_DIR/"
echo ""
echo -e "${YELLOW}Next steps:${NC}"
echo -e "1. Create a GitHub Release: v${SINGBOX_VERSION}"
echo -e "2. Upload these files to the release:"
for ARCH in "${ARCHS[@]}"; do
    echo -e "   - sing-box-linux-${ARCH}"
done
echo ""
echo -e "Release URL: https://github.com/antsbtw/sing-box-docker/releases/new"
