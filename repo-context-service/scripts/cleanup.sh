#!/bin/bash

# Repository Context Service Cleanup Script
# This script cleans up all uploaded repositories, Redis data, and Weaviate data

set -e

echo "🧹 Repository Context Service Cleanup Script"
echo "=========================================="
echo ""

# Color codes for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Function to check if Docker Compose is running
check_docker_compose() {
    if ! docker-compose -f deploy/docker-compose.dev.yml ps | grep -q "Up"; then
        echo -e "${YELLOW}Warning: Docker Compose services don't appear to be running.${NC}"
        echo "This script will still attempt cleanup, but some operations may fail."
        echo ""
    fi
}

# Function to clean up uploaded repository files
cleanup_repository_files() {
    echo -e "${BLUE}🗂️  Cleaning up uploaded repository files...${NC}"

    if [ -d "deploy/repositories" ]; then
        rm -rf deploy/repositories/*
        echo -e "${GREEN}✅ Removed all files from deploy/repositories/${NC}"
    else
        echo -e "${YELLOW}⚠️  deploy/repositories/ directory not found${NC}"
    fi

    # Also clean up any temp files in the container
    if docker ps | grep -q "repo-context-apiserver"; then
        docker exec repo-context-apiserver sh -c 'rm -rf /tmp/uploads/* 2>/dev/null || true'
        echo -e "${GREEN}✅ Cleaned up temporary files in apiserver container${NC}"
    fi
}

# Function to clean up Redis data
cleanup_redis() {
    echo -e "${BLUE}🔴 Cleaning up Redis data...${NC}"

    if docker ps | grep -q "redis"; then
        # Flush all Redis databases
        docker exec repo-context-redis redis-cli FLUSHALL
        echo -e "${GREEN}✅ Flushed all Redis databases${NC}"

        # Show remaining keys (should be empty)
        key_count=$(docker exec repo-context-redis redis-cli DBSIZE)
        echo -e "${GREEN}✅ Redis key count after cleanup: $key_count${NC}"
    else
        echo -e "${RED}❌ Redis container not found or not running${NC}"
    fi
}

# Function to clean up Weaviate data
cleanup_weaviate() {
    echo -e "${BLUE}🟢 Cleaning up Weaviate data...${NC}"

    if docker ps | grep -q "weaviate"; then
        # Get all schema classes (collections) and save to temp file
        curl -s http://localhost:8082/v1/schema | jq -r '.classes[]?.class // empty' 2>/dev/null > /tmp/weaviate_classes.txt || echo "" > /tmp/weaviate_classes.txt

        if [ -s /tmp/weaviate_classes.txt ]; then
            echo "Found Weaviate classes to delete:"
            cat /tmp/weaviate_classes.txt
            echo ""

            # Delete each class
            while IFS= read -r class; do
                if [ -n "$class" ]; then
                    echo "Deleting Weaviate class: $class"
                    curl -s -X DELETE "http://localhost:8082/v1/schema/$class" > /dev/null
                    echo -e "${GREEN}✅ Deleted class: $class${NC}"
                fi
            done < /tmp/weaviate_classes.txt

            # Clean up temp file
            rm -f /tmp/weaviate_classes.txt
        else
            echo -e "${GREEN}✅ No Weaviate classes found (already clean)${NC}"
        fi

        # Verify cleanup
        remaining_classes=$(curl -s http://localhost:8082/v1/schema | jq -r '.classes | length' 2>/dev/null || echo "0")
        echo -e "${GREEN}✅ Remaining Weaviate classes: $remaining_classes${NC}"
    else
        echo -e "${RED}❌ Weaviate container not found or not running${NC}"
    fi
}

# Function to clean up Docker volumes (optional, more aggressive)
cleanup_docker_volumes() {
    echo -e "${BLUE}🐳 Cleaning up Docker volumes (optional)...${NC}"

    echo -e "${YELLOW}This will remove all persistent data. Continue? (y/N)${NC}"
    read -r response

    if [[ "$response" =~ ^[Yy]$ ]]; then
        echo "Stopping Docker Compose services..."
        docker-compose -f deploy/docker-compose.dev.yml down -v
        echo -e "${GREEN}✅ Stopped services and removed volumes${NC}"

        echo "Restarting Docker Compose services..."
        docker-compose -f deploy/docker-compose.dev.yml up -d
        echo -e "${GREEN}✅ Services restarted with clean volumes${NC}"
    else
        echo -e "${YELLOW}⏭️  Skipped Docker volume cleanup${NC}"
    fi
}

# Function to show cleanup summary
show_summary() {
    echo ""
    echo -e "${GREEN}🎉 Cleanup Summary${NC}"
    echo "=================="

    # Check repository files
    if [ -d "deploy/repositories" ] && [ -z "$(ls -A deploy/repositories/)" ]; then
        echo -e "${GREEN}✅ Repository files: Clean${NC}"
    else
        echo -e "${YELLOW}⚠️  Repository files: May still contain data${NC}"
    fi

    # Check Redis
    if docker ps | grep -q "redis"; then
        key_count=$(docker exec repo-context-redis redis-cli DBSIZE 2>/dev/null || echo "unknown")
        if [ "$key_count" = "0" ]; then
            echo -e "${GREEN}✅ Redis: Clean (0 keys)${NC}"
        else
            echo -e "${YELLOW}⚠️  Redis: $key_count keys remaining${NC}"
        fi
    fi

    # Check Weaviate
    if docker ps | grep -q "weaviate"; then
        class_count=$(curl -s http://localhost:8082/v1/schema | jq -r '.classes | length' 2>/dev/null || echo "unknown")
        if [ "$class_count" = "0" ]; then
            echo -e "${GREEN}✅ Weaviate: Clean (0 classes)${NC}"
        else
            echo -e "${YELLOW}⚠️  Weaviate: $class_count classes remaining${NC}"
        fi
    fi

    echo ""
    echo -e "${BLUE}💡 Next steps:${NC}"
    echo "- Upload new repositories via the UI at http://localhost:3000"
    echo "- Check API health at http://localhost:8080/health"
    echo "- View metrics at http://localhost:3001 (Grafana, admin/admin)"
}

# Main cleanup process
main() {
    echo "This script will clean up:"
    echo "- Repository files in deploy/repositories/"
    echo "- All Redis cache data"
    echo "- All Weaviate vector data"
    echo ""
    echo -e "${YELLOW}⚠️  This action cannot be undone!${NC}"
    echo ""
    echo "Continue with cleanup? (y/N)"
    read -r confirm

    if [[ ! "$confirm" =~ ^[Yy]$ ]]; then
        echo "Cleanup cancelled."
        exit 0
    fi

    echo ""
    check_docker_compose

    cleanup_repository_files
    echo ""

    cleanup_redis
    echo ""

    cleanup_weaviate
    echo ""

    # Optionally clean up Docker volumes
    cleanup_docker_volumes

    show_summary

    echo ""
    echo -e "${GREEN}🎉 Cleanup complete!${NC}"
}

# Run main function
main "$@"